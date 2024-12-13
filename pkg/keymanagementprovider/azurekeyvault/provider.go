/*
Copyright The Ratify Authors.
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package azurekeyvault

// This class is based on implementation from  azure secret store csi provider
// Source: https://github.com/Azure/secrets-store-csi-driver-provider-azure/tree/release-1.4/pkg/provider
import (
	"context"
	"crypto"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-jose/go-jose/v3"
	re "github.com/ratify-project/ratify/errors"
	"github.com/ratify-project/ratify/internal/logger"
	"github.com/ratify-project/ratify/pkg/keymanagementprovider"
	"github.com/ratify-project/ratify/pkg/keymanagementprovider/azurekeyvault/types"
	"github.com/ratify-project/ratify/pkg/keymanagementprovider/config"
	"github.com/ratify-project/ratify/pkg/keymanagementprovider/factory"
	"github.com/ratify-project/ratify/pkg/metrics"
	"golang.org/x/crypto/pkcs12"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/runtime"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/keyvault/azcertificates"
	"github.com/Azure/azure-sdk-for-go/sdk/keyvault/azkeys"
	"github.com/Azure/azure-sdk-for-go/sdk/keyvault/azsecrets"
)

const (
	ProviderName               string = "azurekeyvault"
	PKCS12ContentType          string = "application/x-pkcs12"
	PEMContentType             string = "application/x-pem-file"
	versionHistoryLimitDefault int    = 1
)

var logOpt = logger.Option{
	ComponentType: logger.KeyManagementProvider,
}

type AKVKeyManagementProviderConfig struct {
	Type         string                `json:"type"`
	VaultURI     string                `json:"vaultURI"`
	TenantID     string                `json:"tenantID"`
	ClientID     string                `json:"clientID"`
	Resource     string                `json:"resource,omitempty"`
	Certificates []types.KeyVaultValue `json:"certificates,omitempty"`
	Keys         []types.KeyVaultValue `json:"keys,omitempty"`
}

type akvKMProvider struct {
	provider            string
	vaultURI            string
	tenantID            string
	clientID            string
	resource            string
	certificates        []types.KeyVaultValue
	keys                []types.KeyVaultValue
	keyKVClient         keyKVClient
	secretKVClient      secretKVClient
	certificateKVClient certificateKVClient
}

// VersionInfo holds the version and created time of an azure key vault object
type VersionInfo struct {
	Version string
	Created time.Time
	Enabled bool
}

type akvKMProviderFactory struct{}

// kvClient is an interface to interact with the keyvault client used for mocking purposes
type keyKVClient interface {
	// GetKey retrieves a key from the keyvault
	GetKey(ctx context.Context, keyName string, keyVersion string) (azkeys.GetKeyResponse, error)
	// NewListKeyVersionsPager retrieves a pager for listing key versions
	NewListKeyVersionsPager(name string, options *azkeys.ListKeyVersionsOptions) *runtime.Pager[azkeys.ListKeyVersionsResponse]
}
type secretKVClient interface {
	// GetSecret retrieves a secret from the keyvault
	GetSecret(ctx context.Context, secretName string, secretVersion string) (azsecrets.GetSecretResponse, error)
}
type certificateKVClient interface {
	// GetCertificate retrieves a certificate from the keyvault
	GetCertificate(ctx context.Context, certificateName string, certificateVersion string) (azcertificates.GetCertificateResponse, error)
	// NewListCertificateVersionsPager creates a new instance of the ListCertificateVersionsPager
	NewListCertificateVersionsPager(certificateName string, options *azcertificates.ListCertificateVersionsOptions) *runtime.Pager[azcertificates.ListCertificateVersionsResponse]
}

type keyKVClientImpl struct {
	azkeys.Client
}
type secretKVClientImpl struct {
	azsecrets.Client
}
type certificateKVClientImpl struct {
	azcertificates.Client
}

// GetCertificate retrieves a certificate from the keyvault
func (c *certificateKVClientImpl) GetCertificate(ctx context.Context, certificateName string, certificateVersion string) (azcertificates.GetCertificateResponse, error) {
	return c.Client.GetCertificate(ctx, certificateName, certificateVersion, nil)
}

// NewListCertificateVersionsPager retrieves a pager for listing certificate versions
func (c *certificateKVClientImpl) NewListCertificateVersionsPager(certificateName string, options *azcertificates.ListCertificateVersionsOptions) *runtime.Pager[azcertificates.ListCertificateVersionsResponse] {
	return c.Client.NewListCertificateVersionsPager(certificateName, options)
}

// GetKey retrieves a key from the keyvault
func (c *keyKVClientImpl) GetKey(ctx context.Context, keyName string, keyVersion string) (azkeys.GetKeyResponse, error) {
	return c.Client.GetKey(ctx, keyName, keyVersion, nil)
}

// NewListKeyVersionsPager retrieves a pager for listing key versions
func (c *keyKVClientImpl) NewListKeyVersionsPager(keyName string, options *azkeys.ListKeyVersionsOptions) *runtime.Pager[azkeys.ListKeyVersionsResponse] {
	return c.Client.NewListKeyVersionsPager(keyName, options)
}

// GetSecret retrieves a secret from the keyvault
func (c *secretKVClientImpl) GetSecret(ctx context.Context, secretName string, secretVersion string) (azsecrets.GetSecretResponse, error) {
	return c.Client.GetSecret(ctx, secretName, secretVersion, nil)
}

// initKVClient is a function to initialize the keyvault client
// used for mocking purposes
var initKVClient = initializeKvClient

// init calls to register the provider
func init() {
	factory.Register(ProviderName, &akvKMProviderFactory{})
}

// Create creates a new instance of the provider after marshalling and validating the configuration
func (f *akvKMProviderFactory) Create(_ string, keyManagementProviderConfig config.KeyManagementProviderConfig, _ string) (keymanagementprovider.KeyManagementProvider, error) {
	conf := AKVKeyManagementProviderConfig{}

	keyManagementProviderConfigBytes, err := json.Marshal(keyManagementProviderConfig)
	if err != nil {
		return nil, re.ErrorCodeConfigInvalid.WithError(err).WithComponentType(re.KeyManagementProvider)
	}

	if err := json.Unmarshal(keyManagementProviderConfigBytes, &conf); err != nil {
		return nil, re.ErrorCodeConfigInvalid.NewError(re.KeyManagementProvider, "", re.EmptyLink, err, "failed to parse AKV key management provider configuration", re.HideStackTrace)
	}

	if len(conf.Certificates) == 0 && len(conf.Keys) == 0 {
		return nil, re.ErrorCodeConfigInvalid.NewError(re.KeyManagementProvider, ProviderName, re.EmptyLink, nil, "no keyvault certificates or keys configured", re.HideStackTrace)
	}

	provider := &akvKMProvider{
		provider:     ProviderName,
		vaultURI:     strings.TrimSpace(conf.VaultURI),
		tenantID:     strings.TrimSpace(conf.TenantID),
		clientID:     strings.TrimSpace(conf.ClientID),
		certificates: conf.Certificates,
		keys:         conf.Keys,
		resource:     conf.Resource,
	}
	if err := provider.validate(); err != nil {
		return nil, err
	}

	// credProvider is nil, so we will create a new workload identity credential inside the function
	// For testing purposes, we can pass in a mock credential provider
	var credProvider azcore.TokenCredential
	keyKVClient, secretKVClient, certificateKVClient, err := initKVClient(provider.vaultURI, provider.tenantID, provider.clientID, credProvider)
	if err != nil {
		return nil, re.ErrorCodePluginInitFailure.NewError(re.KeyManagementProvider, ProviderName, re.AKVLink, err, "failed to create keyvault client", re.HideStackTrace)
	}

	provider.keyKVClient = &keyKVClientImpl{*keyKVClient}
	provider.secretKVClient = &secretKVClientImpl{*secretKVClient}
	provider.certificateKVClient = &certificateKVClientImpl{*certificateKVClient}

	return provider, nil
}

// GetCertificates returns an array of certificates based on certificate properties defined in config
// get certificate retrieve the entire cert chain using getSecret API call
func (s *akvKMProvider) GetCertificates(ctx context.Context) (map[keymanagementprovider.KMPMapKey][]*x509.Certificate, keymanagementprovider.KeyManagementProviderStatus, error) {
	certsMap := map[keymanagementprovider.KMPMapKey][]*x509.Certificate{}
	certsStatus := []map[string]string{}

	for _, keyVaultCert := range s.certificates {
		if err := s.processCertificate(ctx, keyVaultCert, certsMap, &certsStatus); err != nil {
			return nil, nil, err
		}
	}

	return certsMap, getStatusMap(certsStatus, types.CertificatesStatus), nil
}

func (s *akvKMProvider) processCertificate(ctx context.Context, keyVaultCert types.KeyVaultValue, certsMap map[keymanagementprovider.KMPMapKey][]*x509.Certificate, certsStatus *[]map[string]string) error {
	logger.GetLogger(ctx, logOpt).Debugf("fetching secret from key vault, certName %v, certVersion %v, vaultURI: %v", keyVaultCert.Name, keyVaultCert.Version, s.vaultURI)
	startTime := time.Now()
	if keyVaultCert.VersionHistoryLimit == 0 {
		return s.processCertificateVersion(ctx, keyVaultCert, certsMap, certsStatus, startTime)
	}
	return s.processCertificateVersions(ctx, keyVaultCert, certsMap, certsStatus, startTime)
}

func (s *akvKMProvider) processCertificateVersion(ctx context.Context, keyVaultCert types.KeyVaultValue, certsMap map[keymanagementprovider.KMPMapKey][]*x509.Certificate, certsStatus *[]map[string]string, startTime time.Time) error {

	secretResponse, err := s.secretKVClient.GetSecret(ctx, keyVaultCert.Name, keyVaultCert.Version)
	if err != nil {
		if !isSecretDisabledError(err) {
			return fmt.Errorf("failed to get secret objectName:%s, objectVersion:%s, error: %w", keyVaultCert.Name, keyVaultCert.Version, err)
		}
		return s.handleDisabledSecret(ctx, keyVaultCert, certsStatus, &startTime)
	}

	secretBundle := secretResponse.SecretBundle
	if !isValidSecretBundle(&secretBundle) {
		logger.GetLogger(ctx, logOpt).Warnf("certificate %s, version %s, found invalid secret bundle, attributes or attribute.enabled not be nil", keyVaultCert.Name, keyVaultCert.Version)
		return nil
	}
	isEnabled := *secretBundle.Attributes.Enabled
	version := secretBundle.ID.Version()
	certResult, certProperty, err := getCertsFromSecretBundle(ctx, secretBundle, keyVaultCert.Name, isEnabled)
	if err != nil {
		return fmt.Errorf("failed to get certificates from secret bundle:%w", err)
	}

	metrics.ReportAKVCertificateDuration(ctx, time.Since(startTime).Milliseconds(), keyVaultCert.Name)
	*certsStatus = append(*certsStatus, certProperty...)
	certMapKey := keymanagementprovider.KMPMapKey{Name: keyVaultCert.Name, Version: version, Enabled: isEnabled}
	certsMap[certMapKey] = certResult
	return nil
}

func (s *akvKMProvider) handleDisabledSecret(ctx context.Context, keyVaultCert types.KeyVaultValue, certsStatus *[]map[string]string, startTime *time.Time) error {
	certResponse, err := s.certificateKVClient.GetCertificate(ctx, keyVaultCert.Name, keyVaultCert.Version)
	if err != nil {
		return fmt.Errorf("failed to get certificate objectName:%s, objectVersion:%s, error: %w", keyVaultCert.Name, keyVaultCert.Version, err)
	}

	if !isValidCertificateBundle(&certResponse.CertificateBundle) {
		logger.GetLogger(ctx, logOpt).Warnf(
			"certificate %s, version %s, found invalid certificate bundle, attributes, attribute.enabled or id must not be nil", keyVaultCert.Name, keyVaultCert.Version)
		return nil
	}

	keyVaultCert.Version = certResponse.ID.Version()
	isEnabled := *certResponse.CertificateBundle.Attributes.Enabled
	lastRefreshed := startTime.Format(time.RFC3339)
	certProperty := getStatusProperty(keyVaultCert.Name, keyVaultCert.Version, lastRefreshed, isEnabled)
	*certsStatus = append(*certsStatus, certProperty)
	mapKey := keymanagementprovider.KMPMapKey{Name: keyVaultCert.Name, Version: keyVaultCert.Version, Enabled: isEnabled}
	keymanagementprovider.DeleteCertificateFromMap(s.resource, mapKey) //TODO: unit test
	return nil
}

func (s *akvKMProvider) processCertificateVersions(ctx context.Context, keyVaultCert types.KeyVaultValue, certsMap map[keymanagementprovider.KMPMapKey][]*x509.Certificate, certsStatus *[]map[string]string, startTime time.Time) error {
	versionHistory, err := s.fetchCertificateVersionHistory(ctx, keyVaultCert.Name)
	if err != nil {
		return fmt.Errorf("failed to fetch version history for certificate %s: %w", keyVaultCert.Name, err)
	}

	trimVersionHistory(&versionHistory, keyVaultCert.VersionHistoryLimit, keyVaultCert.Version)

	if len(versionHistory) == 0 {
		logger.GetLogger(ctx, logOpt).Warnf("no versions found for certificate %s", keyVaultCert.Name)
		return nil
	}

	// get the latest versions of the certificate up to the limit
	for _, certVersion := range versionHistory {
		if !certVersion.Enabled {
			lastRefreshed := startTime.Format(time.RFC3339)
			certProperty := getStatusProperty(keyVaultCert.Name, certVersion.Version, lastRefreshed, false)
			*certsStatus = append(*certsStatus, certProperty)
			mapKey := keymanagementprovider.KMPMapKey{Name: keyVaultCert.Name, Version: certVersion.Version, Enabled: false}
			keymanagementprovider.DeleteCertificateFromMap(s.resource, mapKey)
			continue
		}

		secretReponse, err := s.secretKVClient.GetSecret(ctx, keyVaultCert.Name, certVersion.Version)
		if err != nil {
			return fmt.Errorf("failed to get secret objectName:%s, objectVersion:%s, error: %w", keyVaultCert.Name, certVersion.Version, err)
		}

		secretBundle := secretReponse.SecretBundle
		if !isValidSecretBundle(&secretBundle) {
			logger.GetLogger(ctx, logOpt).Warnf("certificate %s, version %s, found invalid secret bundle, attributes or attribute.enabled not be nil", keyVaultCert.Name, certVersion.Version)
			continue
		}

		certResult, certProperty, err := getCertsFromSecretBundle(ctx, secretBundle, keyVaultCert.Name, certVersion.Enabled)
		if err != nil {
			return fmt.Errorf("failed to get certificates from secret bundle:%w", err)
		}

		metrics.ReportAKVCertificateDuration(ctx, time.Since(startTime).Milliseconds(), keyVaultCert.Name)
		*certsStatus = append(*certsStatus, certProperty...)
		certMapKey := keymanagementprovider.KMPMapKey{Name: keyVaultCert.Name, Version: certVersion.Version, Enabled: certVersion.Enabled}
		certsMap[certMapKey] = certResult
	}

	return nil
}

func (s *akvKMProvider) fetchCertificateVersionHistory(ctx context.Context, certName string) (types.KeyVaultValueVersionHistory, error) {
	var versionHistory types.KeyVaultValueVersionHistory
	certVersionPager := s.certificateKVClient.NewListCertificateVersionsPager(certName, &azcertificates.ListCertificateVersionsOptions{})
	for certVersionPager.More() {
		pager, err := certVersionPager.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to get certificate versions for objectName:%s, error: %w", certName, err)
		}
		for _, cert := range pager.Value {
			if !isValidCertificateItem(cert) {
				logger.GetLogger(ctx, logOpt).Warnf("certificate %s, version %s, found invalid certificate object, id, attributes or created time must not be nil", certName, cert.ID.Version())
				continue
			}
			versionInfo := types.KeyVaultValueVersion{
				Version: cert.ID.Version(),
				Created: *cert.Attributes.Created,
				Enabled: *cert.Attributes.Enabled,
			}
			versionHistory = append(versionHistory, versionInfo)
		}
	}

	// Pager results are not sorted by created time, so we sort them here
	// in ascending order (oldest to newest)
	// sortVersionHistory(versionHistory)
	versionHistory.Sort()

	return versionHistory, nil
}

// GetKeys returns an array of keys based on key properties defined in config
func (s *akvKMProvider) GetKeys(ctx context.Context) (map[keymanagementprovider.KMPMapKey]crypto.PublicKey, keymanagementprovider.KeyManagementProviderStatus, error) {
	keysMap := map[keymanagementprovider.KMPMapKey]crypto.PublicKey{}
	keysStatus := []map[string]string{}

	for _, keyVaultKey := range s.keys {
		if err := s.processKey(ctx, keyVaultKey, keysMap, &keysStatus); err != nil {
			return nil, nil, err
		}
	}
	return keysMap, getStatusMap(keysStatus, types.KeysStatus), nil
}

func (s *akvKMProvider) processKey(ctx context.Context, keyVaultKey types.KeyVaultValue, keysMap map[keymanagementprovider.KMPMapKey]crypto.PublicKey, keysStatus *[]map[string]string) error {
	logger.GetLogger(ctx, logOpt).Debugf("fetching key from key vault, keyName %v,  keyvault %v", keyVaultKey.Name, s.vaultURI)
	startTime := time.Now()
	if keyVaultKey.VersionHistoryLimit == 0 {
		return s.processKeyVersion(ctx, keyVaultKey, keysMap, keysStatus, &startTime)
	}
	return s.processKeyVersions(ctx, keyVaultKey, keysMap, keysStatus, &startTime)
}

func (s *akvKMProvider) processKeyVersion(ctx context.Context, keyVaultKey types.KeyVaultValue, keysMap map[keymanagementprovider.KMPMapKey]crypto.PublicKey, keysStatus *[]map[string]string, startTime *time.Time) error {
	keyResponse, err := s.keyKVClient.GetKey(ctx, keyVaultKey.Name, keyVaultKey.Version)
	if err != nil {
		return fmt.Errorf("failed to get key objectName:%s, objectVersion:%s, error: %w", keyVaultKey.Name, keyVaultKey.Version, err)
	}

	keyBundle := keyResponse.KeyBundle
	if !isValidKeyBundle(&keyBundle) {
		logger.GetLogger(ctx, logOpt).Warnf("key %s, version %s, found invalid key bundle, attributes or attribute.enabled not be nil", keyVaultKey.Name, keyVaultKey.Version)
		return nil
	}
	isEnabled := *keyBundle.Attributes.Enabled

	keyVaultKey.Version = getObjectVersion(string(*keyBundle.Key.KID))

	// When the key is disabled, we will remove it from the cache and update the status
	if !isEnabled {
		lastRefreshed := startTime.Format(time.RFC3339)
		properties := getStatusProperty(keyVaultKey.Name, keyVaultKey.Version, lastRefreshed, isEnabled)
		*keysStatus = append(*keysStatus, properties)
		mapKey := keymanagementprovider.KMPMapKey{Name: keyVaultKey.Name, Version: keyVaultKey.Version, Enabled: isEnabled}
		keymanagementprovider.DeleteKeyFromMap(s.resource, mapKey)
		return nil
	}

	publicKey, err := getKeyFromKeyBundle(keyBundle)
	if err != nil {
		return fmt.Errorf("failed to get key from key bundle:%w", err)
	}

	keysMap[keymanagementprovider.KMPMapKey{Name: keyVaultKey.Name, Version: keyVaultKey.Version, Enabled: isEnabled}] = publicKey
	metrics.ReportAKVCertificateDuration(ctx, time.Since(*startTime).Milliseconds(), keyVaultKey.Name)
	properties := getStatusProperty(keyVaultKey.Name, keyVaultKey.Version, time.Now().Format(time.RFC3339), isEnabled)
	*keysStatus = append(*keysStatus, properties)
	return nil
}

func (s *akvKMProvider) processKeyVersions(ctx context.Context, keyVaultKey types.KeyVaultValue, keysMap map[keymanagementprovider.KMPMapKey]crypto.PublicKey, keysStatus *[]map[string]string, startTime *time.Time) error {
	versionHistory, err := s.fetchKeyVersionHistory(ctx, keyVaultKey.Name)
	if err != nil {
		return fmt.Errorf("failed to fetch version history for key %s: %w", keyVaultKey.Name, err)
	}

	trimVersionHistory(&versionHistory, keyVaultKey.VersionHistoryLimit, keyVaultKey.Version)

	if len(versionHistory) == 0 {
		logger.GetLogger(ctx, logOpt).Warnf("no versions found for key %s", keyVaultKey.Name)
		return nil
	}

	// get the latest versions of the key up to the limit
	for _, keyVersion := range versionHistory {
		if !keyVersion.Enabled {
			lastRefreshed := startTime.Format(time.RFC3339)
			properties := getStatusProperty(keyVaultKey.Name, keyVersion.Version, lastRefreshed, false)
			*keysStatus = append(*keysStatus, properties)
			mapKey := keymanagementprovider.KMPMapKey{Name: keyVaultKey.Name, Version: keyVersion.Version, Enabled: false}
			keymanagementprovider.DeleteKeyFromMap(s.resource, mapKey)
			continue
		}

		keyResponse, err := s.keyKVClient.GetKey(ctx, keyVaultKey.Name, keyVersion.Version)
		if err != nil {
			return fmt.Errorf("failed to get key objectName:%s, objectVersion:%s, error: %w", keyVaultKey.Name, keyVersion.Version, err)
		}

		keyBundle := keyResponse.KeyBundle
		if !isValidKeyBundle(&keyBundle) {
			logger.GetLogger(ctx, logOpt).Warnf("key %s, version %s, found invalid key bundle, attributes or attribute.enabled not be nil", keyVaultKey.Name, keyVersion.Version)
			continue
		}

		publicKey, err := getKeyFromKeyBundle(keyBundle)
		if err != nil {
			return fmt.Errorf("failed to get key from key bundle:%w", err)
		}

		keysMap[keymanagementprovider.KMPMapKey{Name: keyVaultKey.Name, Version: keyVersion.Version, Enabled: keyVersion.Enabled}] = publicKey
		properties := getStatusProperty(keyVaultKey.Name, keyVersion.Version, time.Now().Format(time.RFC3339), keyVersion.Enabled)
		*keysStatus = append(*keysStatus, properties)
	}

	return nil
}

func (s *akvKMProvider) fetchKeyVersionHistory(ctx context.Context, keyName string) (types.KeyVaultValueVersionHistory, error) {
	var versionHistory types.KeyVaultValueVersionHistory
	keyVersionPager := s.keyKVClient.NewListKeyVersionsPager(keyName, &azkeys.ListKeyVersionsOptions{})
	for keyVersionPager.More() {
		pager, err := keyVersionPager.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to get key versions for objectName:%s, error: %w", keyName, err)
		}
		for _, key := range pager.Value {
			if !isValidKeyItem(key) {
				logger.GetLogger(ctx, logOpt).Warnf("key %s, version %s, invalid: ID, attributes, created time, or enabled field must not be nil.", keyName, key.KID.Version())
				continue
			}
			versionInfo := types.KeyVaultValueVersion{
				Version: key.KID.Version(),
				Created: *key.Attributes.Created,
				Enabled: *key.Attributes.Enabled,
			}
			versionHistory = append(versionHistory, versionInfo)
		}
	}

	// Pager results are not sorted by created time, so we sort them here
	// in ascending order (oldest to newest)
	versionHistory.Sort()

	return versionHistory, nil
}

func (s *akvKMProvider) IsRefreshable() bool {
	return true
}

// azure keyvault provider certificate/key status is a map from "certificates" key or "keys" key to an array of key management provider status
func getStatusMap(statusMap []map[string]string, contentType string) keymanagementprovider.KeyManagementProviderStatus {
	status := keymanagementprovider.KeyManagementProviderStatus{}
	status[contentType] = statusMap
	return status
}

// return a status object that consist of the cert/key name, version, enabled and last refreshed time
func getStatusProperty(name, version, lastRefreshed string, enabled bool) map[string]string {
	properties := map[string]string{}
	properties[types.StatusName] = name
	properties[types.StatusVersion] = version
	properties[types.StatusEnabled] = strconv.FormatBool(enabled)
	properties[types.StatusLastRefreshed] = lastRefreshed
	return properties
}

// initializeKvClient creates a new keyvault client for keys, secrets and certificates
// TODO: credProvider in only added to params for testing purposes. Make sure it is handled properly in future
func initializeKvClient(keyVaultURI, tenantID, clientID string, credProvider azcore.TokenCredential) (*azkeys.Client, *azsecrets.Client, *azcertificates.Client, error) {
	// Trim any trailing slash from the endpoint
	kvEndpoint := strings.TrimSuffix(keyVaultURI, "/")

	// If credProvider is nil, create the default credential
	if credProvider == nil {
		var err error
		credProvider, err = azidentity.NewWorkloadIdentityCredential(&azidentity.WorkloadIdentityCredentialOptions{
			ClientID: clientID,
			TenantID: tenantID,
		})
		if err != nil {
			return nil, nil, nil, re.ErrorCodeAuthDenied.WithDetail("failed to create workload identity credential").WithError(err)
		}
	}

	// create azkeys client
	keyKVClient, err := azkeys.NewClient(kvEndpoint, credProvider, nil)
	if err != nil {
		return nil, nil, nil, re.ErrorCodeConfigInvalid.WithDetail("Failed to create keys Key Vault client").WithError(err)
	}

	// create azsecrets client
	secretKVClient, err := azsecrets.NewClient(kvEndpoint, credProvider, nil)
	if err != nil {
		return nil, nil, nil, re.ErrorCodeConfigInvalid.WithDetail("Failed to create secrets Key Vault client").WithError(err)
	}

	// create azcertificates client
	certificateKVClient, err := azcertificates.NewClient(kvEndpoint, credProvider, nil)
	if err != nil {
		return nil, nil, nil, re.ErrorCodeConfigInvalid.WithDetail("Failed to create certificates Key Vault client").WithError(err)
	}

	return keyKVClient, secretKVClient, certificateKVClient, nil
}

// Parse the secret bundle and return an array of certificates
// In a certificate chain scenario, all certificates from root to leaf will be returned
func getCertsFromSecretBundle(ctx context.Context, secretBundle azsecrets.SecretBundle, certName string, enabled bool) ([]*x509.Certificate, []map[string]string, error) {
	version := getObjectVersion(string(*secretBundle.ID))

	// This aligns with notation akv implementation
	// akv plugin supports both PKCS12 and PEM. https://github.com/Azure/notation-azure-kv/blob/558e7345ef8318783530de6a7a0a8420b9214ba8/Notation.Plugin.AzureKeyVault/KeyVault/KeyVaultClient.cs#L192
	if *secretBundle.ContentType != PKCS12ContentType &&
		*secretBundle.ContentType != PEMContentType {
		return nil, nil, re.ErrorCodeCertInvalid.NewError(re.KeyManagementProvider, ProviderName, re.EmptyLink, nil, fmt.Sprintf("certificate %s version %s, unsupported secret content type %s, supported type are %s and %s", certName, version, *secretBundle.ContentType, PKCS12ContentType, PEMContentType), re.HideStackTrace)
	}

	results := []*x509.Certificate{}
	certsStatus := []map[string]string{}
	lastRefreshed := time.Now().Format(time.RFC3339)

	data := []byte(*secretBundle.Value)

	if *secretBundle.ContentType == PKCS12ContentType {
		p12, err := base64.StdEncoding.DecodeString(*secretBundle.Value)
		if err != nil {
			return nil, nil, re.ErrorCodeCertInvalid.NewError(re.KeyManagementProvider, ProviderName, re.EmptyLink, err, fmt.Sprintf("azure keyvault key management provider: failed to decode PKCS12 Value. Certificate %s, version %s", certName, version), re.HideStackTrace)
		}

		blocks, err := pkcs12.ToPEM(p12, "")
		if err != nil {
			return nil, nil, re.ErrorCodeCertInvalid.NewError(re.KeyManagementProvider, ProviderName, re.EmptyLink, err, fmt.Sprintf("azure keyvault key management provider: failed to convert PKCS12 Value to PEM. Certificate %s, version %s", certName, version), re.HideStackTrace)
		}

		var pemData []byte
		for _, b := range blocks {
			pemData = append(pemData, pem.EncodeToMemory(b)...)
		}
		data = pemData
	}

	block, rest := pem.Decode(data)

	for block != nil {
		switch block.Type {
		case "PRIVATE KEY":
			logger.GetLogger(ctx, logOpt).Warnf("azure keyvault key management provider: certificate %s, version %s private key skipped. Please see doc to learn how to create a new certificate in keyvault with non exportable keys. https://learn.microsoft.com/en-us/azure/key-vault/certificates/how-to-export-certificate?tabs=azure-cli#exportable-and-non-exportable-keys", certName, version)
		case "CERTIFICATE":
			var pemData []byte
			pemData = append(pemData, pem.EncodeToMemory(block)...)
			decodedCerts, err := keymanagementprovider.DecodeCertificates(pemData)
			if err != nil {
				return nil, nil, re.ErrorCodeCertInvalid.NewError(re.KeyManagementProvider, ProviderName, re.EmptyLink, err, fmt.Sprintf("azure keyvault key management provider: failed to decode Certificate %s, version %s", certName, version), re.HideStackTrace)
			}
			for _, cert := range decodedCerts {
				results = append(results, cert)
				certProperty := getStatusProperty(certName, version, lastRefreshed, enabled)
				certsStatus = append(certsStatus, certProperty)
			}
		default:
			logger.GetLogger(ctx, logOpt).Warnf("certificate '%s', version '%s': azure keyvault key management provider detected unknown block type %s", certName, version, block.Type)
		}

		block, rest = pem.Decode(rest)
		if block == nil && len(rest) > 0 {
			return nil, nil, re.ErrorCodeCertInvalid.NewError(re.KeyManagementProvider, ProviderName, re.EmptyLink, nil, fmt.Sprintf("certificate '%s', version '%s': azure keyvault key management provider error, block is nil and remaining block to parse > 0", certName, version), re.HideStackTrace)
		}
	}
	logger.GetLogger(ctx, logOpt).Debugf("azurekeyvault certprovider getCertsFromSecretBundle: %v certificates parsed, Certificate '%s', version '%s'", len(results), certName, version)
	return results, certsStatus, nil
}

// Based on https://github.com/sigstore/sigstore/blob/8b208f7d608b80a7982b2a66358b8333b1eec542/pkg/signature/kms/azure/client.go#L258
func getKeyFromKeyBundle(keyBundle azkeys.KeyBundle) (crypto.PublicKey, error) {
	webKey := keyBundle.Key
	if webKey == nil {
		return nil, re.ErrorCodeKeyInvalid.NewError(re.KeyManagementProvider, ProviderName, re.EmptyLink, nil, "found invalid key bundle, key must not be nil", re.HideStackTrace)
	}

	if webKey.Kty == nil {
		return nil, re.ErrorCodeKeyInvalid.NewError(re.KeyManagementProvider, ProviderName, re.EmptyLink, nil, "found invalid key bundle, keytype must not be nil", re.HideStackTrace)
	}

	keyType := *webKey.Kty
	switch keyType {
	case azkeys.JSONWebKeyTypeECHSM:
		ecType := azkeys.JSONWebKeyTypeEC
		webKey.Kty = &ecType
	case azkeys.JSONWebKeyTypeRSAHSM:
		rsaType := azkeys.JSONWebKeyTypeRSA
		webKey.Kty = &rsaType
	}

	keyBytes, err := json.Marshal(webKey)
	if err != nil {
		return nil, re.ErrorCodeKeyInvalid.NewError(re.KeyManagementProvider, ProviderName, re.EmptyLink, err, "failed to marshal key", re.HideStackTrace)
	}

	key := jose.JSONWebKey{}
	err = key.UnmarshalJSON(keyBytes)
	if err != nil {
		return nil, re.ErrorCodeKeyInvalid.NewError(re.KeyManagementProvider, ProviderName, re.EmptyLink, err, "failed to unmarshal key into JSON Web Key", re.HideStackTrace)
	}

	return key.Key, nil
}

// getObjectVersion parses the id to retrieve the version
// of object fetched
// example id format - https://kindkv.vault.azure.net/secrets/actual/1f304204f3624873aab40231241243eb
// TODO (aramase) follow up on https://github.com/Azure/azure-rest-api-specs/issues/10825 to provide
// a native way to obtain the version
func getObjectVersion(id string) string {
	splitID := strings.Split(id, "/")
	return splitID[len(splitID)-1]
}

func isSecretDisabledError(err error) bool {
	// AzureError defines the structure of the error response from Azure Key Vault
	// This structure is defined according to https://learn.microsoft.com/en-us/rest/api/keyvault/keys/get-keys/get-keys?view=rest-keyvault-keys-7.4&tabs=HTTP#error
	type AzureError struct {
		Error struct {
			Code       string `json:"code"`
			Message    string `json:"message"`
			InnerError struct {
				Code string `json:"code"`
			} `json:"innererror"`
		} `json:"error"`
	}

	// Parse err and make sure it is a secretDisabled error and return true
	const ErrorCodeForbidden = "Forbidden"
	const SecretDisabledCode = "SecretDisabled"
	var httpErr *azcore.ResponseError
	if errors.As(err, &httpErr) {
		if httpErr.StatusCode != http.StatusForbidden {
			return false
		}

		var azureError AzureError
		errorResponseBody, readErr := io.ReadAll(httpErr.RawResponse.Body)
		if readErr != nil {
			return false
		}
		jsonErr := json.Unmarshal(errorResponseBody, &azureError)
		if jsonErr == nil && azureError.Error.Code == ErrorCodeForbidden && azureError.Error.InnerError.Code == SecretDisabledCode {
			return true
		}
	}

	// Return false if it's not a secretDisabled error
	return false
}

func trimVersionHistory(versionHistory *types.KeyVaultValueVersionHistory, limit int, version string) {
	if version == "" {
		//Ensure that the versionHistoryLimit is not greater than the number of versions to avoid out of bounds error
		if limit > len(*versionHistory) {
			limit = len(*versionHistory)
		}

		// Shorten the version history to the limit
		*versionHistory = (*versionHistory)[len(*versionHistory)-limit:]
		return
	}
	// Find the index of the specified version
	versionIndex := -1
	for i, v := range *versionHistory {
		if v.Version == version {
			versionIndex = i
			break
		}
	}

	// Calculate the start index to include up to `versionHistoryLimit` older versions
	start := versionIndex - limit
	if start < 0 {
		start = 0 // Ensure we don't go out of bounds
	}

	// Create the slice including the specified version as the tail
	*versionHistory = (*versionHistory)[start : versionIndex+1]
}

func isValidCertificateBundle(certBundle *azcertificates.CertificateBundle) bool {
	return certBundle != nil && certBundle.Attributes != nil && certBundle.Attributes.Enabled != nil && certBundle.ID != nil
}

func isValidSecretBundle(secretBundle *azsecrets.SecretBundle) bool {
	return secretBundle != nil && secretBundle.Attributes != nil && secretBundle.Attributes.Enabled != nil && secretBundle.ContentType != nil && secretBundle.Value != nil && secretBundle.ID != nil
}

func isValidCertificateItem(cert *azcertificates.CertificateItem) bool {
	return cert != nil && cert.ID != nil && cert.Attributes != nil && cert.Attributes.Created != nil && cert.Attributes.Enabled != nil
}

func isValidKeyBundle(keyBundle *azkeys.KeyBundle) bool {
	return keyBundle != nil && keyBundle.Attributes != nil && keyBundle.Attributes.Enabled != nil
}

func isValidKeyItem(key *azkeys.KeyItem) bool {
	return key != nil && key.KID != nil && key.Attributes != nil && key.Attributes.Created != nil && key.Attributes.Enabled != nil
}

// validate checks vaultURI, tenantID, clientID are set and all certificates/keys have a name
func (s *akvKMProvider) validate() error {
	if s.vaultURI == "" {
		return re.ErrorCodeConfigInvalid.NewError(re.KeyManagementProvider, ProviderName, re.EmptyLink, nil, "vaultURI is not set", re.HideStackTrace)
	}
	if s.tenantID == "" {
		return re.ErrorCodeConfigInvalid.NewError(re.KeyManagementProvider, ProviderName, re.EmptyLink, nil, "tenantID is not set", re.HideStackTrace)
	}
	if s.clientID == "" {
		return re.ErrorCodeConfigInvalid.NewError(re.KeyManagementProvider, ProviderName, re.EmptyLink, nil, "clientID is not set", re.HideStackTrace)
	}

	// all certificates must have a name
	for i := range s.certificates {
		if s.certificates[i].Name == "" {
			return re.ErrorCodeConfigInvalid.NewError(re.KeyManagementProvider, ProviderName, re.EmptyLink, nil, fmt.Sprintf("name is not set for the %d th certificate", i+1), re.HideStackTrace)
		}
	}

	// all keys must have a name
	for i := range s.keys {
		if s.keys[i].Name == "" {
			return re.ErrorCodeConfigInvalid.NewError(re.KeyManagementProvider, ProviderName, re.EmptyLink, nil, fmt.Sprintf("name is not set for the %d th key", i+1), re.HideStackTrace)
		}
	}

	return nil
}
