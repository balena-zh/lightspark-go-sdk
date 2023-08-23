// Copyright ©, 2023-present, Lightspark Group, Inc. - All Rights Reserved
package uma

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
	eciesgo "github.com/ecies/go/v2"
	"github.com/lightsparkdev/go-sdk/objects"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// FetchPublicKeyForVasp fetches the public key for another VASP.
//
// If the public key is not in the cache, it will be fetched from the VASP's domain.
// The public key will be cached for future use.
//
// Args:
//
//	vaspDomain: the domain of the VASP.
//	cache: the PublicKeyCache cache to use. You can use the InMemoryPublicKeyCache struct, or implement your own persistent cache with any storage type.
func FetchPublicKeyForVasp(vaspDomain string, cache PublicKeyCache) (*PubKeyResponse, error) {
	publicKey := cache.FetchPublicKeyForVasp(vaspDomain)
	if publicKey != nil {
		return publicKey, nil
	}

	scheme := "https://"
	if strings.HasPrefix(vaspDomain, "localhost:") {
		scheme = "http://"
	}
	resp, err := http.Get(scheme + vaspDomain + "/.well-known/lnurlpubkey")
	if err != nil {
		return nil, err
	}

	defer func(Body io.ReadCloser) {
		err := Body.Close()
		if err != nil {
			panic(err)
		}
	}(resp.Body)

	if resp.StatusCode != 200 {
		return nil, errors.New("invalid response from VASP")
	}

	responseBodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var pubKeyResponse PubKeyResponse
	err = json.Unmarshal(responseBodyBytes, &pubKeyResponse)
	if err != nil {
		return nil, err
	}

	cache.AddPublicKeyForVasp(vaspDomain, &pubKeyResponse)
	return &pubKeyResponse, nil
}

func GenerateNonce() (*string, error) {
	randomBigInt, err := rand.Int(rand.Reader, big.NewInt(0xFFFFFFFF))
	if err != nil {
		return nil, err
	}
	nonce := strconv.FormatUint(randomBigInt.Uint64(), 10)
	return &nonce, nil
}

func signPayload(payload []byte, privateKeyBytes []byte) (*string, error) {
	privateKey := secp256k1.PrivKeyFromBytes(privateKeyBytes)
	if privateKey == nil {
		return nil, errors.New("invalid private key")
	}
	hashedPayload := sha256.Sum256(payload)
	signature, err := privateKey.ToECDSA().Sign(rand.Reader, hashedPayload[:], nil)
	if err != nil {
		return nil, err
	}
	signatureString := hex.EncodeToString(signature)
	return &signatureString, nil
}

// VerifyPayReqSignature Verifies the signature on a uma pay request based on the public key of the VASP making the request.
//
// Args:
//
//	query: the signed query to verify.
//	otherVaspPubKey: the bytes of the signing public key of the VASP making this request.
func VerifyPayReqSignature(query *PayRequest, otherVaspPubKey []byte) error {
	hashedPayload := sha256.Sum256(query.signablePayload())
	return verifySignature(hashedPayload, query.PayerData.Compliance.Signature, otherVaspPubKey)
}

// verifySignature Verifies the signature of the uma request.
//
// Args:
//
//	hashedPayload: the sha256 hash of the payload.
//	signature: the hex-encoded signature.
//	otherVaspPubKey: the bytes of the signing public key of the VASP who signed the payload.
func verifySignature(hashedPayload [32]byte, signature string, otherVaspPubKey []byte) error {
	decodedSignature, err := hex.DecodeString(signature)
	if err != nil {
		return err
	}
	otherVaspPubKeyParsed, err := secp256k1.ParsePubKey(otherVaspPubKey)
	if err != nil {
		return err
	}

	sigParsed, err := ecdsa.ParseDERSignature(decodedSignature)
	if err != nil {
		return err
	}

	verified := sigParsed.Verify(hashedPayload[:], otherVaspPubKeyParsed)

	if !verified {
		return errors.New("invalid uma signature")
	}

	return nil
}

// GetSignedLnurlpRequestUrl Creates a signed uma request URL.
//
// Args:
//
//	signingPrivateKey: the private key of the VASP that is sending the payment. This will be used to sign the request.
//	receiverAddress: the address of the receiver of the payment (i.e. $bob@vasp2).
//	senderVaspDomain: the domain of the VASP that is sending the payment. It will be used by the receiver to fetch the public keys of the sender.
//	trStatus: whether the sending VASP is a financial institution that requires travel rule information.
func GetSignedLnurlpRequestUrl(
	signingPrivateKey []byte,
	receiverAddress string,
	senderVaspDomain string,
	trStatus bool,
) (*url.URL, error) {
	nonce, err := GenerateNonce()
	if err != nil {
		return nil, err
	}
	unsignedRequest := LnurlpRequest{
		ReceiverAddress: receiverAddress,
		TrStatus:        trStatus,
		VaspDomain:      senderVaspDomain,
		Timestamp:       time.Now(),
		Nonce:           *nonce,
	}
	signature, err := signPayload(unsignedRequest.signablePayload(), signingPrivateKey)
	if err != nil {
		return nil, err
	}
	unsignedRequest.Signature = *signature

	return unsignedRequest.EncodeToUrl()
}

// IsUmaLnurlpQuery Checks if the given URL is a valid UMA request.
func IsUmaLnurlpQuery(url url.URL) bool {
	query, err := ParseLnurlpRequest(url)
	return err == nil && query != nil
}

// ParseLnurlpRequest Parse Parses the message into an LnurlpRequest object.
// Args:
//
//	url: the full URL of the uma request.
func ParseLnurlpRequest(url url.URL) (*LnurlpRequest, error) {
	query := url.Query()
	signature := query.Get("signature")
	vaspDomain := query.Get("vaspDomain")
	nonce := query.Get("nonce")
	trStatus := query.Get("trStatus")
	timestamp := query.Get("timestamp")
	timestampAsString, dateErr := strconv.ParseInt(timestamp, 10, 64)
	if dateErr != nil {
		return nil, errors.New("invalid timestamp")
	}
	timestampAsTime := time.Unix(timestampAsString, 0)

	if vaspDomain == "" || signature == "" || nonce == "" || timestamp == "" {
		return nil, errors.New("missing uma query parameters. vaspDomain, signature, nonce, and timestamp are required")
	}

	pathParts := strings.Split(url.Path, "/")
	if len(pathParts) != 4 || pathParts[1] != ".well-known" || pathParts[2] != "lnurlp" {
		return nil, errors.New("invalid uma request path")
	}
	receiverAddress := pathParts[3] + "@" + url.Host

	return &LnurlpRequest{
		VaspDomain:      vaspDomain,
		Signature:       signature,
		ReceiverAddress: receiverAddress,
		Nonce:           nonce,
		Timestamp:       timestampAsTime,
		TrStatus:        strings.ToLower(trStatus) == "true",
	}, nil
}

// VerifyUmaLnurlpQuerySignature Verifies the signature on an uma Lnurlp query based on the public key of the VASP making the request.
//
// Args:
//
//	query: the signed query to verify.
//	otherVaspSigningPubKey: the public key of the VASP making this request in bytes.
func VerifyUmaLnurlpQuerySignature(query *LnurlpRequest, otherVaspSigningPubKey []byte) error {
	hashedPayload := sha256.Sum256(query.signablePayload())
	return verifySignature(hashedPayload, query.Signature, otherVaspSigningPubKey)
}

func GetLnurlpResponse(
	query *LnurlpRequest,
	privateKeyBytes []byte,
	requiresTravelRuleInfo bool,
	callback string,
	encodedMetadata string,
	minSendableSats int64,
	maxSendableSats int64,
	payerDataOptions PayerDataOptions,
	currencyOptions []Currency,
	isReceiverKYCd bool,
) (*LnurlpResponse, error) {
	complianceResponse, err := getSignedLnurlpComplianceResponse(query, privateKeyBytes, requiresTravelRuleInfo, isReceiverKYCd)
	if err != nil {
		return nil, err
	}
	return &LnurlpResponse{
		Tag:               "payRequest",
		Callback:          callback,
		MinSendable:       minSendableSats,
		MaxSendable:       maxSendableSats,
		EncodedMetadata:   encodedMetadata,
		Currencies:        currencyOptions,
		RequiredPayerData: payerDataOptions,
		Compliance:        *complianceResponse,
	}, nil
}

func getSignedLnurlpComplianceResponse(
	query *LnurlpRequest,
	privateKeyBytes []byte,
	trStatus bool,
	isReceiverKYCd bool,
) (*LnurlComplianceResponse, error) {
	timestamp := time.Now().Unix()
	nonce, err := GenerateNonce()
	if err != nil {
		return nil, err
	}
	payloadString := strings.Join([]string{query.ReceiverAddress, *nonce, strconv.FormatInt(timestamp, 10)}, "|")
	signature, err := signPayload([]byte(payloadString), privateKeyBytes)
	if err != nil {
		return nil, err
	}
	return &LnurlComplianceResponse{
		IsKYCd:             isReceiverKYCd,
		Signature:          *signature,
		Nonce:              *nonce,
		Timestamp:          timestamp,
		TrStatus:           trStatus,
		ReceiverIdentifier: query.ReceiverAddress,
	}, nil
}

// VerifyUmaLnurlpResponseSignature Verifies the signature on an uma Lnurlp response based on the public key of the VASP making the request.
//
// Args:
//
//	response: the signed response to verify.
//	otherVaspSigningPubKey: the public key of the VASP making this request in bytes.
func VerifyUmaLnurlpResponseSignature(response *LnurlpResponse, otherVaspSigningPubKey []byte) error {
	hashedPayload := sha256.Sum256(response.signablePayload())
	return verifySignature(hashedPayload, response.Compliance.Signature, otherVaspSigningPubKey)
}

func ParseLnurlpResponse(bytes []byte) (*LnurlpResponse, error) {
	var response LnurlpResponse
	err := json.Unmarshal(bytes, &response)
	if err != nil {
		return nil, err
	}
	return &response, nil
}

// GetPayRequest Creates a signed uma pay request.
//
// Args:
//
//	receiverEncryptionPubKey: the public key of the receiver that will be used to encrypt the travel rule information.
//	sendingVaspPrivateKey: the private key of the VASP that is sending the payment. This will be used to sign the request.
//	currencyCode: the code of the currency that the receiver will receive for this payment.
//	amount: the amount of the payment in the smallest unit of the specified currency (i.e. cents for USD).
//	payerIdentifier: the identifier of the sender. For example, $alice@vasp1.com
//	payerName: the name of the sender (optional).
//	payerEmail: the email of the sender (optional).
//	trInfo: the travel rule information. This will be encrypted before sending to the receiver.
//	isPayerKYCd: whether the sender is a KYC'd customer of the sending VASP.
//	payerUtxos: the list of UTXOs of the sender's channels that might be used to fund the payment.
//	utxoCallback: the URL that the receiver will call to send UTXOs of the channel that the receiver used to receive the payment once it completes.
func GetPayRequest(
	receiverEncryptionPubKey []byte,
	sendingVaspPrivateKey []byte,
	currencyCode string,
	amount int64,
	payerIdentifier string,
	payerName *string,
	payerEmail *string,
	trInfo *string,
	isPayerKYCd bool,
	payerUtxos *[]string,
	utxoCallback string,
) (*PayRequest, error) {
	complianceData, err := getSignedCompliancePayerData(
		receiverEncryptionPubKey,
		sendingVaspPrivateKey,
		payerIdentifier,
		trInfo,
		isPayerKYCd,
		payerUtxos,
		utxoCallback,
	)
	if err != nil {
		return nil, err
	}

	return &PayRequest{
		CurrencyCode: currencyCode,
		Amount:       amount,
		PayerData: PayerData{
			Name:       payerName,
			Email:      payerEmail,
			Identifier: payerIdentifier,
			Compliance: complianceData,
		},
	}, nil
}

func getSignedCompliancePayerData(
	receiverEncryptionPubKeyBytes []byte,
	sendingVaspPrivateKeyBytes []byte,
	payerIdentifier string,
	trInfo *string,
	isPayerKYCd bool,
	payerUtxos *[]string,
	utxoCallback string,
) (*CompliancePayerData, error) {
	timestamp := time.Now().Unix()
	nonce, err := GenerateNonce()
	if err != nil {
		return nil, err
	}
	var encryptedTrInfo *string
	if trInfo != nil {
		encryptedTrInfo, err = encryptTrInfo(*trInfo, receiverEncryptionPubKeyBytes)
		if err != nil {
			return nil, err
		}
	}
	payloadString := strings.Join([]string{payerIdentifier, *nonce, strconv.FormatInt(timestamp, 10)}, "|")
	signature, err := signPayload([]byte(payloadString), sendingVaspPrivateKeyBytes)
	if err != nil {
		return nil, err
	}

	return &CompliancePayerData{
		TrInfo:             encryptedTrInfo,
		IsKYCd:             isPayerKYCd,
		Utxos:              payerUtxos,
		UtxoCallback:       utxoCallback,
		SignatureNonce:     *nonce,
		SignatureTimestamp: timestamp,
		Signature:          *signature,
	}, nil
}

func encryptTrInfo(trInfo string, receiverEncryptionPubKey []byte) (*string, error) {
	pubKey, err := eciesgo.NewPublicKeyFromBytes(receiverEncryptionPubKey)
	if err != nil {
		return nil, err
	}

	encryptedTrInfoBytes, err := eciesgo.Encrypt(pubKey, []byte(trInfo))
	if err != nil {
		return nil, err
	}

	encryptedTrInfoHex := hex.EncodeToString(encryptedTrInfoBytes)
	return &encryptedTrInfoHex, nil
}

func ParsePayRequest(bytes []byte) (*PayRequest, error) {
	var response PayRequest
	err := json.Unmarshal(bytes, &response)
	if err != nil {
		return nil, err
	}
	return &response, nil
}

type LnurlInvoiceCreator interface {
	CreateLnurlInvoice(nodeId string, masterSeedBytes *[]byte, amountMsats int64, metadata string, expirySecs *int32) (*objects.Invoice, error)
}

// GetPayReqResponse Creates an uma pay request response with an encoded invoice.
//
// Args:
//
//		query: the uma pay request.
//		invoiceCreator: the object that will create the invoice. In practice, this is usually a `services.LightsparkClient`.
//		nodeId: the node ID of the receiver.
//	 	nodeMasterSeedBytes: the master seed bytes of the receiver.
//		metadata: the metadata that will be added to the invoice's metadata hash field
//		currencyCode: the code of the currency that the receiver will receive for this payment.
//		conversionRate: milli-satoshis per the smallest unit of the specified currency. This rate is committed to by the
//	    	receiving VASP until the invoice expires.
//		expirySecs: the number of seconds until the invoice expires.
//		receiverChannelUtxos: the list of UTXOs of the receiver's channels that might be used to fund the payment.
//		utxoCallback: the URL that the receiving VASP will call to send UTXOs of the channel that the receiver used to
//	    	receive the payment once it completes.
func GetPayReqResponse(
	query *PayRequest,
	invoiceCreator LnurlInvoiceCreator,
	nodeId string,
	nodeMasterSeedBytes *[]byte,
	metadata string,
	currencyCode string,
	conversionRate int64,
	expirySecs int32,
	receiverChannelUtxos []string,
	utxoCallback string,
) (*PayReqResponse, error) {
	msatsAmount := query.Amount * conversionRate
	encodedPayerData, err := json.Marshal(query.PayerData)
	if err != nil {
		return nil, err
	}
	encodedInvoice, err := invoiceCreator.CreateLnurlInvoice(nodeId, nodeMasterSeedBytes, msatsAmount, metadata+"{"+string(encodedPayerData)+"}", &expirySecs)
	if err != nil {
		return nil, err
	}
	return &PayReqResponse{
		EncodedInvoice: encodedInvoice.Data.EncodedPaymentRequest,
		Routes:         []Route{},
		Compliance: PayReqResponseCompliance{
			Utxos:        receiverChannelUtxos,
			UtxoCallback: utxoCallback,
		},
		PaymentInfo: PayReqResponsePaymentInfo{
			CurrencyCode: currencyCode,
			Multiplier:   conversionRate,
		},
	}, nil
}

func ParsePayReqResponse(bytes []byte) (*PayReqResponse, error) {
	var response PayReqResponse
	err := json.Unmarshal(bytes, &response)
	if err != nil {
		return nil, err
	}
	return &response, nil
}

// Protocol TODO list:
// VASP1: EncodeLnurlpRequest   x
// VASP2: ParseLnurlpRequest    x
// VASP2: GetLnurlpResponse     x
// VASP1: ParseLnurlpResponse   x
// VASP1: EncodePayRequest      x -- TODO: Need to update signature
// VASP2: ParsePayRequest       x
// VASP2: GetPayReqResponse     x
// VASP1: ParsePayReqResponse   x
// Both: handlePubKeyRequest	x // only in example

// Post-transaction hooks
