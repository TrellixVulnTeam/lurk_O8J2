package ca

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/asn1"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io/ioutil"
	"math/big"
	"net/http"
	//"net"
	"os"
	"os/exec"
	"strings"
	"time"

	cfsslConfig "github.com/cloudflare/cfssl/config"
	cferr "github.com/cloudflare/cfssl/errors"
	"github.com/cloudflare/cfssl/ocsp"
	"github.com/cloudflare/cfssl/signer"
	"github.com/cloudflare/cfssl/signer/local"
	"github.com/jmhodges/clock"
	"github.com/miekg/pkcs11"
	"golang.org/x/net/context"

	caPB "github.com/letsencrypt/boulder/ca/proto"
	"github.com/letsencrypt/boulder/cmd"
	"github.com/letsencrypt/boulder/core"
	csrlib "github.com/letsencrypt/boulder/csr"
	berrors "github.com/letsencrypt/boulder/errors"
	"github.com/letsencrypt/boulder/features"
	"github.com/letsencrypt/boulder/goodkey"
	blog "github.com/letsencrypt/boulder/log"
	"github.com/letsencrypt/boulder/metrics"
	"github.com/prometheus/client_golang/prometheus"
)

// Miscellaneous PKIX OIDs that we need to refer to
var (
	// X.509 Extensions
	oidAuthorityInfoAccess    = asn1.ObjectIdentifier{1, 3, 6, 1, 5, 5, 7, 1, 1}
	oidAuthorityKeyIdentifier = asn1.ObjectIdentifier{2, 5, 29, 35}
	oidBasicConstraints       = asn1.ObjectIdentifier{2, 5, 29, 19}
	oidCertificatePolicies    = asn1.ObjectIdentifier{2, 5, 29, 32}
	oidCrlDistributionPoints  = asn1.ObjectIdentifier{2, 5, 29, 31}
	oidExtKeyUsage            = asn1.ObjectIdentifier{2, 5, 29, 37}
	oidKeyUsage               = asn1.ObjectIdentifier{2, 5, 29, 15}
	oidSubjectAltName         = asn1.ObjectIdentifier{2, 5, 29, 17}
	oidSubjectKeyIdentifier   = asn1.ObjectIdentifier{2, 5, 29, 14}
	oidTLSFeature             = asn1.ObjectIdentifier{1, 3, 6, 1, 5, 5, 7, 1, 24}

	// CSR attribute requesting extensions
	oidExtensionRequest = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 9, 14}
)

var port_98_STAROpen = false
//var port_87_STAROpen = false

// OID and fixed value for the "must staple" variant of the TLS Feature
// extension:
//
//  Features ::= SEQUENCE OF INTEGER                  [RFC7633]
//  enum { ... status_request(5) ...} ExtensionType;  [RFC6066]
//
// DER Encoding:
//  30 03 - SEQUENCE (3 octets)
//  |-- 02 01 - INTEGER (1 octet)
//  |   |-- 05 - 5
var (
	mustStapleFeatureValue = []byte{0x30, 0x03, 0x02, 0x01, 0x05}
	mustStapleExtension    = signer.Extension{
		ID:       cfsslConfig.OID(oidTLSFeature),
		Critical: false,
		Value:    hex.EncodeToString(mustStapleFeatureValue),
	}
)

// Metrics for CA statistics
const (
	// Increments when CA observes an HSM or signing error
	metricSigningError = "SigningError"
	metricHSMError     = metricSigningError + ".HSMError"

	csrExtensionCategory          = "category"
	csrExtensionBasic             = "basic"
	csrExtensionTLSFeature        = "tls-feature"
	csrExtensionTLSFeatureInvalid = "tls-feature-invalid"
	csrExtensionOther             = "other"
)

type certificateStorage interface {
	AddCertificate(context.Context, []byte, int64, []byte) (string, error)
}

// CertificateAuthorityImpl represents a CA that signs certificates, CRLs, and
// OCSP responses.
type CertificateAuthorityImpl struct {
	rsaProfile   string
	ecdsaProfile string
	// A map from issuer cert common name to an internalIssuer struct
	issuers map[string]*internalIssuer
	// The common name of the default issuer cert
	defaultIssuer     *internalIssuer
	sa                certificateStorage
	pa                core.PolicyAuthority
	keyPolicy         goodkey.KeyPolicy
	clk               clock.Clock
	log               blog.Logger
	stats             metrics.Scope
	prefix            int // Prepended to the serial number
	validityPeriod    time.Duration
	maxNames          int
	forceCNFromSAN    bool
	enableMustStaple  bool
	signatureCount    *prometheus.CounterVec
	csrExtensionCount *prometheus.CounterVec
}

// Issuer represents a single issuer certificate, along with its key.
type Issuer struct {
	Signer crypto.Signer
	Cert   *x509.Certificate
}

// internalIssuer represents the fully initialized internal state for a single
// issuer, including the cfssl signer and OCSP signer objects.
type internalIssuer struct {
	cert       *x509.Certificate
	eeSigner   signer.Signer
	ocspSigner ocsp.Signer
}

func makeInternalIssuers(
	issuers []Issuer,
	policy *cfsslConfig.Signing,
	lifespanOCSP time.Duration,
) (map[string]*internalIssuer, error) {
	if len(issuers) == 0 {
		return nil, errors.New("No issuers specified.")
	}
	internalIssuers := make(map[string]*internalIssuer)
	for _, iss := range issuers {
		if iss.Cert == nil || iss.Signer == nil {
			return nil, errors.New("Issuer with nil cert or signer specified.")
		}
		eeSigner, err := local.NewSigner(iss.Signer, iss.Cert, x509.SHA256WithRSA, policy)
		if err != nil {
			return nil, err
		}

		// Set up our OCSP signer. Note this calls for both the issuer cert and the
		// OCSP signing cert, which are the same in our case.
		ocspSigner, err := ocsp.NewSigner(iss.Cert, iss.Cert, iss.Signer, lifespanOCSP)
		if err != nil {
			return nil, err
		}
		cn := iss.Cert.Subject.CommonName
		if internalIssuers[cn] != nil {
			return nil, errors.New("Multiple issuer certs with the same CommonName are not supported")
		}
		internalIssuers[cn] = &internalIssuer{
			cert:       iss.Cert,
			eeSigner:   eeSigner,
			ocspSigner: ocspSigner,
		}
	}
	return internalIssuers, nil
}

// NewCertificateAuthorityImpl creates a CA instance that can sign certificates
// from a single issuer (the first first in the issuers slice), and can sign OCSP
// for any of the issuer certificates provided.
func NewCertificateAuthorityImpl(
	config cmd.CAConfig,
	sa certificateStorage,
	pa core.PolicyAuthority,
	clk clock.Clock,
	stats metrics.Scope,
	issuers []Issuer,
	keyPolicy goodkey.KeyPolicy,
	logger blog.Logger,
) (*CertificateAuthorityImpl, error) {
	var ca *CertificateAuthorityImpl
	var err error

	if config.SerialPrefix <= 0 || config.SerialPrefix >= 256 {
		err = errors.New("Must have a positive non-zero serial prefix less than 256 for CA.")
		return nil, err
	}

	// CFSSL requires processing JSON configs through its own LoadConfig, so we
	// serialize and then deserialize.
	cfsslJSON, err := json.Marshal(config.CFSSL)
	if err != nil {
		return nil, err
	}
	cfsslConfigObj, err := cfsslConfig.LoadConfig(cfsslJSON)
	if err != nil {
		return nil, err
	}

	if config.LifespanOCSP.Duration == 0 {
		return nil, errors.New("Config must specify an OCSP lifespan period.")
	}

	internalIssuers, err := makeInternalIssuers(
		issuers,
		cfsslConfigObj.Signing,
		config.LifespanOCSP.Duration)
	if err != nil {
		return nil, err
	}
	defaultIssuer := internalIssuers[issuers[0].Cert.Subject.CommonName]

	rsaProfile := config.RSAProfile
	ecdsaProfile := config.ECDSAProfile

	if rsaProfile == "" || ecdsaProfile == "" {
		return nil, errors.New("must specify rsaProfile and ecdsaProfile")
	}

	csrExtensionCount := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "csrExtensions",
			Help: "Number of CSRs with extensions of the given category",
		},
		[]string{csrExtensionCategory})
	stats.MustRegister(csrExtensionCount)

	signatureCount := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "signatures",
			Help: "Number of signatures",
		},
		[]string{"purpose"})
	stats.MustRegister(signatureCount)

	ca = &CertificateAuthorityImpl{
		sa:                sa,
		pa:                pa,
		issuers:           internalIssuers,
		defaultIssuer:     defaultIssuer,
		rsaProfile:        rsaProfile,
		ecdsaProfile:      ecdsaProfile,
		prefix:            config.SerialPrefix,
		clk:               clk,
		log:               logger,
		stats:             stats,
		keyPolicy:         keyPolicy,
		forceCNFromSAN:    !config.DoNotForceCN, // Note the inversion here
		enableMustStaple:  config.EnableMustStaple,
		signatureCount:    signatureCount,
		csrExtensionCount: csrExtensionCount,
	}

	if config.Expiry == "" {
		return nil, errors.New("Config must specify an expiry period.")
	}
	ca.validityPeriod, err = time.ParseDuration(config.Expiry)
	if err != nil {
		return nil, err
	}

	ca.maxNames = config.MaxNames

	return ca, nil
}

// noteSignError is called after operations that may cause a CFSSL
// or PKCS11 signing error.
func (ca *CertificateAuthorityImpl) noteSignError(err error) {
	if err != nil {
		if _, ok := err.(*pkcs11.Error); ok {
			ca.stats.Inc(metricHSMError, 1)
		} else if cfErr, ok := err.(*cferr.Error); ok {
			ca.stats.Inc(fmt.Sprintf("%s.%d", metricSigningError, cfErr.ErrorCode), 1)
		}
	}
	return
}

// Extract supported extensions from a CSR.  The following extensions are
// currently supported:
//
// * 1.3.6.1.5.5.7.1.24 - TLS Feature [RFC7633], with the "must staple" value.
//                        Any other value will result in an error.
//
// Other requested extensions are silently ignored.
func (ca *CertificateAuthorityImpl) extensionsFromCSR(csr *x509.CertificateRequest) ([]signer.Extension, error) {
	extensions := []signer.Extension{}

	extensionSeen := map[string]bool{}
	hasBasic := false
	hasOther := false

	for _, attr := range csr.Attributes {
		if !attr.Type.Equal(oidExtensionRequest) {
			continue
		}

		for _, extList := range attr.Value {
			for _, ext := range extList {
				if extensionSeen[ext.Type.String()] {
					// Ignore duplicate certificate extensions
					continue
				}
				extensionSeen[ext.Type.String()] = true

				switch {
				case ext.Type.Equal(oidTLSFeature):
					ca.csrExtensionCount.With(prometheus.Labels{csrExtensionCategory: csrExtensionTLSFeature}).Inc()
					value, ok := ext.Value.([]byte)
					if !ok {
						return nil, berrors.MalformedError("malformed extension with OID %v", ext.Type)
					} else if !bytes.Equal(value, mustStapleFeatureValue) {
						ca.csrExtensionCount.With(prometheus.Labels{csrExtensionCategory: csrExtensionTLSFeatureInvalid}).Inc()
						return nil, berrors.MalformedError("unsupported value for extension with OID %v", ext.Type)
					}

					if ca.enableMustStaple {
						extensions = append(extensions, mustStapleExtension)
					}
				case ext.Type.Equal(oidAuthorityInfoAccess),
					ext.Type.Equal(oidAuthorityKeyIdentifier),
					ext.Type.Equal(oidBasicConstraints),
					ext.Type.Equal(oidCertificatePolicies),
					ext.Type.Equal(oidCrlDistributionPoints),
					ext.Type.Equal(oidExtKeyUsage),
					ext.Type.Equal(oidKeyUsage),
					ext.Type.Equal(oidSubjectAltName),
					ext.Type.Equal(oidSubjectKeyIdentifier):
					hasBasic = true
				default:
					hasOther = true
				}
			}
		}
	}

	if hasBasic {
		ca.csrExtensionCount.With(prometheus.Labels{csrExtensionCategory: csrExtensionBasic}).Inc()
	}

	if hasOther {
		ca.csrExtensionCount.With(prometheus.Labels{csrExtensionCategory: csrExtensionOther}).Inc()
	}

	return extensions, nil
}

// GenerateOCSP produces a new OCSP response and returns it
func (ca *CertificateAuthorityImpl) GenerateOCSP(ctx context.Context, xferObj core.OCSPSigningRequest) ([]byte, error) {
	cert, err := x509.ParseCertificate(xferObj.CertDER)
	if err != nil {
		ca.log.AuditErr(err.Error())
		return nil, err
	}

	signRequest := ocsp.SignRequest{
		Certificate: cert,
		Status:      xferObj.Status,
		Reason:      int(xferObj.Reason),
		RevokedAt:   xferObj.RevokedAt,
	}

	cn := cert.Issuer.CommonName
	issuer := ca.issuers[cn]
	if issuer == nil {
		return nil, fmt.Errorf("This CA doesn't have an issuer cert with CommonName %q", cn)
	}

	err = cert.CheckSignatureFrom(issuer.cert)
	if err != nil {
		return nil, fmt.Errorf("GenerateOCSP was asked to sign OCSP for cert "+
			"%s from %q, but the cert's signature was not valid: %s.",
			core.SerialToString(cert.SerialNumber), cn, err)
	}

	ocspResponse, err := issuer.ocspSigner.Sign(signRequest)
	ca.noteSignError(err)
	if err == nil {
		ca.signatureCount.With(prometheus.Labels{"purpose": "ocsp"}).Inc()
	}
	return ocspResponse, err
}
/*
Returns not-before, not-after
*/
func getCertDates(fulluri string)(startdate, enddate string){

	pathToCert := "./starCerts/" + fulluri +"/certificate.pem"

	checkEndDate := []string{"x509", "-in", pathToCert, "-noout", "-enddate"}
	ex_enddate, err := exec.Command("openssl", checkEndDate...).Output()
	if err != nil{
		panic(err)
	}

	checkStartDate := []string{"x509", "-in", pathToCert, "-noout", "-startdate"}
        ex_startdate, err := exec.Command("openssl", checkStartDate...).Output()
        if err != nil{
                panic(err)
        }
	//These are in format: notAfter=May... so split after the '=' to only get the date	
	startdate = (string)(ex_enddate)
	enddate = (string)(ex_startdate)

	startdate = strings.SplitAfter(startdate, "=")[1]
	enddate = strings.SplitAfter(enddate, "=")[1]
	return
	


}

//Keeps ALL the certs posted at port 9898
func postAtUuid(completionURL_value string) {
	 fulluri := "/" + completionURL_value
	 fmt.Printf("CA.GO: Serving certificate at: %s", completionURL_value)
	 http.HandleFunc(fulluri, func(w http.ResponseWriter, r *http.Request) {
		
		//First checks if file is still being served.
		_, err := os.Stat("./starCerts/" + completionURL_value + "/certificate.pem")
			if err != nil{
				w.WriteHeader(http.StatusNotFound)
				fmt.Fprint(w, "Order status: canceled")
		
			}else{

		//Calculate cert's not-before & not-after in real time. Then sets them as headers.
		notB, notA := getCertDates(fulluri)
                w.Header().Add("Not-Before", notB)
		w.Header().Add("Not-After", notA)

 	        http.ServeFile(w, r, "./starCerts/" + completionURL_value + "/certificate.pem")
		}
          }) 

   if port_98_STAROpen == false {
	port_98_STAROpen = true
	fmt.Printf("CA.GO: PORT STATUS IS: true")
	err := http.ListenAndServeTLS(":9898","cert.pem","key.pem", nil)
    		if err != nil {
        		panic(err)
    		}
   }

}
//Expects GETs to the certID. Returns the uuid decided by the CA that serves as URI.
func postURIAtCertsID(certID string, certUUID string) {
	 fullUri := "/" + certID
	 fileServed := "./starCerts/" + certUUID + "/renewalURI.txt"
	 fmt.Printf("CA.GO: Uri is available at: %s and it contains %s END", fullUri, fileServed)
         http.HandleFunc(fullUri, func(w http.ResponseWriter, r *http.Request) {
                http.ServeFile(w, r, fileServed)
          })
	
        //err := http.ListenAndServeTLS(":37987","cert.pem","key.pem", nil)

}

/*
Adds auto renewal using a cronjob if STAR request was successful
it deletes itself when lifetime. To change the time of the renewal
go to exeAutoRenew.sh
*/
func addStarToCron (crtUuid string) {

	domainBytes, _ := ioutil.ReadFile("STARDomainWFE")
        domainStr := string(domainBytes[0:len(domainBytes)] )
	
	lifeTimeBytes, _ := ioutil.ReadFile("STARLifeTimeWFE")
        lifeTimeStr := string(lifeTimeBytes[0:len(lifeTimeBytes)] )
	fmt.Printf("CA.GO: domain name, lifetime, crtuuid: %s, %s, %s", domainStr, lifeTimeStr, crtUuid)	
	
	//Creates a file that will be handled by the renewalManager in a different thread
	renewKeyInfo := domainStr + " " + lifeTimeStr + " " + crtUuid
	toFileErr := ioutil.WriteFile("./renewTmp/renew1", []byte(renewKeyInfo), 0777)
                if toFileErr != nil {
                        panic(toFileErr)
                }

}


/*Removes tmp files used in STAR
It executes after the renewal has been added and all the information is storaged
*/
func rmSTARtmpFiles () {
                cantRm := os.Remove("STARValidityWFE")
                if cantRm != nil {
                        panic(cantRm)
                }
                /*cantRm = os.Remove("STARUuidWFE") //RA still needs this file
                if cantRm != nil {
                        panic(cantRm)
                }
		*/
                cantRm = os.Remove("STARLifeTimeWFE")
                if cantRm != nil {
                        panic(cantRm)
                }
		
                cantRm = os.Remove("STARDomainWFE")
                if cantRm != nil {
                        panic(cantRm)
                }
		
		
}



// IssueCertificate attempts to convert a CSR into a signed Certificate, while
// enforcing all policies. Names (domains) in the CertificateRequest will be
// lowercased before storage.
// Currently it will always sign with the defaultIssuer.
func (ca *CertificateAuthorityImpl) IssueCertificate(ctx context.Context, issueReq *caPB.IssueCertificateRequest) (core.Certificate, error) {
	emptyCert := core.Certificate{}
	
	//Checks if STARValidity exists, in that case it reads and then removes the file to apply STAR protocol
        _, err2 := os.Stat("STARValidityWFE")
	var crtUuidStr string
        if err2 == nil {
		crtExpBytes, _ := ioutil.ReadFile("STARValidityWFE")
		crtExpStr := string(crtExpBytes[0:len(crtExpBytes)] )
		fmt.Printf("\nCA.GO: STAR protocol = true with crtExpStr => %s h", crtExpStr)

		myMap := ca.issuers["h2ppy h2cker fake CA"].eeSigner.Policy()
        	myMap.Profiles["rsaEE"].Expiry, _   =   time.ParseDuration(crtExpStr + "h")
	        myMap.Profiles["ecdsaEE"].Expiry, _ =   time.ParseDuration(crtExpStr + "h")

		_, err2 = os.Stat("STARUuidWFE")
		if err2 == nil {
			crtUuidBytes, _ := ioutil.ReadFile("STARUuidWFE")
                	crtUuidStr = string(crtUuidBytes[0:len(crtUuidBytes)])
			go postAtUuid(crtUuidStr)
			addStarToCron(crtUuidStr)
			
			 //Adds the cronjob that will renew the cert
	                 addStarToCron(crtUuidStr)
	                 //rmSTARtmpFiles ()
		}
		/*_, err2 = os.Stat("STARCertsIDWFE")
		if err2 == nil {
			//crtIDBytes, _ := ioutil.ReadFile("STARCertsIDWFE")
			//crtID := string(crtIDBytes[0:len(crtIDBytes)])
			go postURIAtCertsID()
		}*/

        } else if err2 != nil {
		fmt.Println("CA.GO: STAR protocol = false")
		myMap := ca.issuers["h2ppy h2cker fake CA"].eeSigner.Policy()
        	myMap.Profiles["rsaEE"].Expiry, _ = time.ParseDuration("2160h")
	        myMap.Profiles["ecdsaEE"].Expiry, _ = time.ParseDuration("2160h")

	}


	if issueReq.RegistrationID == nil {
		return emptyCert, berrors.InternalServerError("RegistrationID is nil")
	}
	regID := *issueReq.RegistrationID

	notAfter, serialBigInt, err := ca.generateNotAfterAndSerialNumber()
	if err != nil {
		return emptyCert, err
	}

	certDER, err := ca.issueCertificateOrPrecertificate(ctx, issueReq, notAfter, serialBigInt, "cert")
	if err != nil {
		return emptyCert, err
	}

	cert := core.Certificate{
		DER: certDER,
	}

	var ocspResp []byte
	if features.Enabled(features.GenerateOCSPEarly) {
		ocspResp, err = ca.GenerateOCSP(ctx, core.OCSPSigningRequest{
			CertDER: certDER,
			Status:  "good",
		})
		if err != nil {
			err = berrors.InternalServerError(err.Error())
			ca.log.AuditInfo(fmt.Sprintf("OCSP Signing failure: serial=[%s] err=[%s]", core.SerialToString(serialBigInt), err))
			// Ignore errors here to avoid orphaning the certificate. The
			// ocsp-updater will look for certs with a zero ocspLastUpdated
			// and generate the initial response in this case.
		}
	}

	// Store the cert with the certificate authority, if provided
	_, err = ca.sa.AddCertificate(ctx, certDER, regID, ocspResp)
	if err != nil {
		err = berrors.InternalServerError(err.Error())
		// Note: This log line is parsed by cmd/orphan-finder. If you make any
		// changes here, you should make sure they are reflected in orphan-finder.
		ca.log.AuditErr(fmt.Sprintf(
			"Failed RPC to store at SA, orphaning certificate: serial=[%s] cert=[%s] err=[%v], regID=[%d]",
			core.SerialToString(serialBigInt),
			hex.EncodeToString(certDER),
			err,
			regID,
		))
		return emptyCert, err
	}

	return cert, nil
}

func (ca *CertificateAuthorityImpl) generateNotAfterAndSerialNumber() (time.Time, *big.Int, error) {
	notAfter := ca.clk.Now().Add(ca.validityPeriod)

	// We want 136 bits of random number, plus an 8-bit instance id prefix.
	const randBits = 136
	serialBytes := make([]byte, randBits/8+1)
	serialBytes[0] = byte(ca.prefix)
	_, err := rand.Read(serialBytes[1:])
	if err != nil {
		err = berrors.InternalServerError("failed to generate serial: %s", err)
		ca.log.AuditErr(fmt.Sprintf("Serial randomness failed, err=[%v]", err))
		return time.Time{}, nil, err
	}
	serialBigInt := big.NewInt(0)
	serialBigInt = serialBigInt.SetBytes(serialBytes)

	return notAfter, serialBigInt, nil
}

func (ca *CertificateAuthorityImpl) issueCertificateOrPrecertificate(ctx context.Context, issueReq *caPB.IssueCertificateRequest, notAfter time.Time, serialBigInt *big.Int, certType string) ([]byte, error) {
	csr, err := x509.ParseCertificateRequest(issueReq.Csr)
	if err != nil {
		return nil, err
	}

	if err := csrlib.VerifyCSR(
		csr,
		ca.maxNames,
		&ca.keyPolicy,
		ca.pa,
		ca.forceCNFromSAN,
		*issueReq.RegistrationID,
	); err != nil {
		ca.log.AuditErr(err.Error())
		return nil, berrors.MalformedError(err.Error())
	}

	requestedExtensions, err := ca.extensionsFromCSR(csr)
	if err != nil {
		return nil, err
	}

	issuer := ca.defaultIssuer

	if issuer.cert.NotAfter.Before(notAfter) {
		err = berrors.InternalServerError("cannot issue a certificate that expires after the issuer certificate")
		ca.log.AuditErr(err.Error())
		return nil, err
	}

	// Convert the CSR to PEM
	csrPEM := string(pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE REQUEST",
		Bytes: csr.Raw,
	}))

	var profile string
	switch csr.PublicKey.(type) {
	case *rsa.PublicKey:
		profile = ca.rsaProfile
	case *ecdsa.PublicKey:
		profile = ca.ecdsaProfile
	default:
		err = berrors.InternalServerError("unsupported key type %T", csr.PublicKey)
		ca.log.AuditErr(err.Error())
		return nil, err
	}

	// Send the cert off for signing
	req := signer.SignRequest{
		Request: csrPEM,
		Profile: profile,
		Hosts:   csr.DNSNames,
		Subject: &signer.Subject{
			CN: csr.Subject.CommonName,
		},
		Serial:     serialBigInt,
		Extensions: requestedExtensions,
	}

	serialHex := core.SerialToString(serialBigInt)

	if !ca.forceCNFromSAN {
		req.Subject.SerialNumber = serialHex
	}

	ca.log.AuditInfo(fmt.Sprintf("Signing: serial=[%s] names=[%s] csr=[%s]",
		serialHex, strings.Join(csr.DNSNames, ", "), hex.EncodeToString(csr.Raw)))

	certPEM, err := issuer.eeSigner.Sign(req)
	ca.noteSignError(err)
	if err != nil {
		err = berrors.InternalServerError("failed to sign certificate: %s", err)
		ca.log.AuditErr(fmt.Sprintf("Signing failed: serial=[%s] err=[%v]", serialHex, err))
		return nil, err
	}
	ca.signatureCount.With(prometheus.Labels{"purpose": certType}).Inc()

	if len(certPEM) == 0 {
		err = berrors.InternalServerError("no certificate returned by server")
		ca.log.AuditErr(fmt.Sprintf("PEM empty from Signer: serial=[%s] err=[%v]", serialHex, err))
		return nil, err
	}

	block, _ := pem.Decode(certPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		err = berrors.InternalServerError("invalid certificate value returned")
		ca.log.AuditErr(fmt.Sprintf("PEM decode error, aborting: serial=[%s] pem=[%s] err=[%v]",
			serialHex, certPEM, err))
		return nil, err
	}
	certDER := block.Bytes

	ca.log.AuditInfo(fmt.Sprintf("Signing success: serial=[%s] names=[%s] csr=[%s] %s=[%s]",
		serialHex, strings.Join(csr.DNSNames, ", "), hex.EncodeToString(csr.Raw), certType,
		hex.EncodeToString(certDER)))

	//Saves the original cert's ID for STAR
	
	_, err = os.Stat("STARUuidWFE")
                if err == nil {
                        crtUuidBytes, _ := ioutil.ReadFile("STARUuidWFE")
                        crtUuidStr := string(crtUuidBytes[0:len(crtUuidBytes)])
			go postURIAtCertsID(serialHex, crtUuidStr)
			rmSTARtmpFiles()
	
	}

	/*
	fileID, err := os.OpenFile("STARCertsIDWFE", os.O_APPEND|os.O_WRONLY, 0666)
	defer fileID.Close()
	
	if _, err = fileID.WriteString(serialHex); err != nil {
                    panic(err)
        }
	*/
	return certDER, nil
}
