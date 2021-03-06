package server

import (
	"crypto/rsa"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	log "github.com/Sirupsen/logrus"

	"github.com/rancher/go-rancher/client"
	"github.com/rancher/rancher-auth-service/model"
	"github.com/rancher/rancher-auth-service/providers"
	"github.com/rancher/rancher-auth-service/util"
)

const (
	accessModeSetting = "api.auth.github.access.mode"
	allowedIdentitiesSetting = "api.auth.github.allowed.identities"
	providerSetting = "api.auth.provider.configured"
	providerNameSetting = "api.auth.provider.name.configured"
	securitySetting = "api.security.enabled"
)

var (
	provider       providers.IdentityProvider
	privateKey     *rsa.PrivateKey
	publicKey      *rsa.PublicKey
	authConfigInMemory 	   model.AuthConfig
	rancherClient  *client.RancherClient
	debug          = flag.Bool("debug", false, "Debug")
	logFile        = flag.String("log", "", "Log file")
	publicKeyFile  = flag.String("publicKeyFile", "", "Path of file containing RSA Public key")
	privateKeyFile = flag.String("privateKeyFile", "", "Path of file containing RSA Private key")
)

//SetEnv sets the parameters necessary
func SetEnv() {
	flag.Parse()

	textFormatter := &log.TextFormatter{
		FullTimestamp: true,
	}
	log.SetFormatter(textFormatter)

	if *debug {
		log.SetLevel(log.DebugLevel)
	}

	if *publicKeyFile == "" {
		log.Fatal("Please provide the RSA public key, halting")
		return
	}
	publicKey = util.ParsePublicKey(*publicKeyFile)

	if *privateKeyFile == "" {
		log.Fatal("Please provide the RSA private key, halting")
		return
	}
	privateKey = util.ParsePrivateKey(*privateKeyFile)

	cattleURL := os.Getenv("CATTLE_URL")
	if len(cattleURL) == 0 {
		log.Fatalf("CATTLE_URL is not set")
	}

	cattleAPIKey := os.Getenv("CATTLE_ACCESS_KEY")
	if len(cattleAPIKey) == 0 {
		log.Fatalf("CATTLE_ACCESS_KEY is not set")
	}

	cattleSecretKey := os.Getenv("CATTLE_SECRET_KEY")
	if len(cattleSecretKey) == 0 {
		log.Fatalf("CATTLE_SECRET_KEY is not set")
	}

	//configure cattle client
	var err error
	rancherClient, err = newCattleClient(cattleURL, cattleAPIKey, cattleSecretKey)
	if err != nil {
		log.Fatalf("Failed to configure cattle client: %v", err)
	}

	err = testCattleConnect()
	if err != nil {
		log.Errorf("Failed to connect to rancher cattle client: %v", err)
	}
}

func newCattleClient(cattleURL string, cattleAccessKey string, cattleSecretKey string) (*client.RancherClient, error) {
	apiClient, err := client.NewRancherClient(&client.ClientOpts{
		Url:       cattleURL,
		AccessKey: cattleAccessKey,
		SecretKey: cattleSecretKey,
	})

	if err != nil {
		return nil, err
	}

	return apiClient, nil
}

func testCattleConnect() error {
	opts := &client.ListOpts{}
	_, err := rancherClient.ContainerEvent.List(opts)
	return err
}


func initProviderWithConfig(authConfig model.AuthConfig) (providers.IdentityProvider, error) {
	newProvider := providers.GetProvider(authConfig.Provider)
	if newProvider == nil {
		return nil, fmt.Errorf("Could not get the %s auth provider", authConfig.Provider)
	}
	err := newProvider.LoadConfig(authConfig)
	if err != nil {
		log.Debugf("Error Loading the provider config %v", err)
		return nil, err
	}
	return newProvider, nil
}

func readSettings(settings []string) (map[string]string, error) {
	var dbSettings = make(map[string]string)
	
	for _, key := range settings {
		setting, err := rancherClient.Setting.ById(key)
		if err != nil {
			log.Errorf("Error reading the setting %v , error: %v", key, err)
			return dbSettings, err
		}
		dbSettings[key] = setting.ActiveValue
	}
	
	return dbSettings, nil
}

func updateSettings(settings map[string]string) error {
	for key, value := range settings {
		if value != "" {
			setting, err := rancherClient.Setting.ById(key)
			if err != nil {
				log.Errorf("Error getting the setting %v , error: %v", key, err)
				return err
			}	
			setting, err = rancherClient.Setting.Update(setting, &client.Setting{
				Value: value,
			})
			if err != nil {
				log.Errorf("Error updating the setting %v to value %v, error: %v", key, value, err)
				return err
			}
		}
	}
	return nil
}

func getAllowedIDString(allowedIdentities []client.Identity) string {
	if provider != nil {
		var idArray []string
		for _, identity := range allowedIdentities {
			idArray = append(idArray, identity.Id)
		}
		return strings.Join(idArray, ",")
	}
	return ""
}

func getAllowedIdentities(idString string, accessToken string) []client.Identity {
	var identities []client.Identity
	if idString != "" {
		log.Debugf("idString %v", idString)
		externalIDList := strings.Split(idString, ",")
	
		for _, id := range externalIDList {
			var identity client.Identity
			var err error
			parts := strings.SplitN(id, ":", 2)
			
			if len(parts) < 2 {
				log.Debugf("Malformed Id, skipping this allowed identity %v", id)
				continue
			}

			if provider != nil && accessToken != "" {
				//get identities from the provider
				identity, err = provider.GetIdentity(parts[1], parts[0], accessToken)
				if err == nil {
					identities = append(identities, identity)
					continue
				}
			}
	
			identity = client.Identity{Resource: client.Resource{
				Type: "identity",
			}}
			identity.ExternalId = parts[1]
			identity.Resource.Id = id
			identity.ExternalIdType = parts[0]
			identities = append(identities, identity)
		}
	}
	
	return identities
}

//UpdateConfig updates the config in DB
func UpdateConfig(authConfig model.AuthConfig) error {

	newProvider, err := initProviderWithConfig(authConfig)
	if err != nil {
		log.Errorf("UpdateConfig: Cannot update the config, error initializing the provider %v", err)
		return err
	}
	//store the config to db
	providerSettings := newProvider.GetSettings()

	//add the generic settings
	providerSettings[accessModeSetting] = authConfig.AccessMode
	providerSettings[allowedIdentitiesSetting] = getAllowedIDString(authConfig.AllowedIdentities)
	providerSettings[securitySetting] = strconv.FormatBool(authConfig.Enabled)
	providerSettings[providerNameSetting] = authConfig.Provider
	if authConfig.Enabled {
		providerSettings[providerSetting] = authConfig.Provider
	}
	err = updateSettings(providerSettings)
	if err != nil {
		log.Errorf("Error Storing the provider settings %v", err)
		return err
	}
	//switch the in-memory provider 
	provider = newProvider
	authConfigInMemory = authConfig
	
	return nil
}

//GetConfig gets the config from DB, gathers the list of settings to read from DB
func GetConfig(accessToken string) (model.AuthConfig, error) {
	var config model.AuthConfig
	var settings []string

	config = model.AuthConfig{Resource: client.Resource{
			Type: "config",
		}}

	//add the generic settings
	settings = append(settings, accessModeSetting)
	settings = append(settings, allowedIdentitiesSetting)
	settings = append(settings, securitySetting)
	settings = append(settings, providerSetting)
	settings = append(settings, providerNameSetting)
	
	dbSettings, err := readSettings(settings)
	
	if err != nil {
		log.Errorf("GetConfig: Error reading DB settings %v", err)
		return config, err
	}
	
	config.AccessMode = dbSettings[accessModeSetting]
	config.AllowedIdentities = getAllowedIdentities(dbSettings[allowedIdentitiesSetting], accessToken)
	enabled, err := strconv.ParseBool(dbSettings[securitySetting])
	if err == nil {
		config.Enabled = enabled
	} else {
		config.Enabled  = false
	}
	
	providerNameInDb := dbSettings[providerNameSetting]
	
	log.Debugf("Provider Name In Db %v", providerNameInDb)
	
	config.Provider = providerNameInDb
	
	//add the provider specific config
	newProvider := providers.GetProvider(config.Provider)
	if newProvider == nil {
		return config, fmt.Errorf("Could not get the %s auth provider", config.Provider)
	}	
	providerSettings, err := readSettings(newProvider.GetProviderSettingList())	
	newProvider.AddProviderConfig(&config, providerSettings)
	
	
	return config, nil
}

//Reload will reload the config from DB and reinit the provider
func Reload() error {
	//read config from db
	authConfig, err := GetConfig("")
	
	newProvider, err := initProviderWithConfig(authConfig)
	if err != nil {
		log.Errorf("Error initializing the provider %v", err)
		return err
	}
	provider = newProvider
	authConfigInMemory = authConfig	
	return nil
}

//CreateToken will authenticate with provider and create a jwt token
func CreateToken(securityCode string) (string, error) {
	if provider != nil {
		token, err := provider.GenerateToken(securityCode)
		if err != nil {
			return "", err
		}
	
		payload := make(map[string]interface{})
		payload["token"] = token.Type
		payload["account_id"] = token.ExternalAccountID
		payload["access_token"] = token.AccessToken
		payload["idList"] = identitiesToIDList(token.IdentityList)
		payload["identities"] = token.IdentityList
	
		return util.CreateTokenWithPayload(payload, privateKey)
	} 
	return "", fmt.Errorf("No auth provider configured")
}

//RefreshToken will refresh a jwt token
func RefreshToken(accessToken string) (string, error) {
	if provider != nil {
		token, err := provider.RefreshToken(accessToken)
		if err != nil {
			return "", err
		}
	
		payload := make(map[string]interface{})
		payload["token"] = token.Type
		payload["account_id"] = token.ExternalAccountID
		payload["access_token"] = token.AccessToken
		payload["idList"] = identitiesToIDList(token.IdentityList)
		payload["identities"] = token.IdentityList
	
		return util.CreateTokenWithPayload(payload, privateKey)
	} 
	return "", fmt.Errorf("No auth provider configured")
}

func identitiesToIDList(identities []client.Identity) []string {
	var idList []string
	for _, identity := range identities {
		idList = append(idList, identity.Resource.Id)
	}
	return idList
}

//GetIdentities will list all identities for token
func GetIdentities(accessToken string) ([]client.Identity, error) {
	if provider != nil {
		return provider.GetIdentities(accessToken)
	}
	return []client.Identity{}, fmt.Errorf("No auth provider configured")
}

//GetIdentity will list all identities for given filters
func GetIdentity(externalID string, externalIDType string, accessToken string) (client.Identity, error) {
	if provider != nil {
		return provider.GetIdentity(externalID, externalIDType, accessToken)
	}
	return client.Identity{}, fmt.Errorf("No auth provider configured")
}

//SearchIdentities will list all identities for given filters
func SearchIdentities(name string, exactMatch bool, accessToken string) ([]client.Identity, error) {
	if provider != nil {
		return provider.SearchIdentities(name, exactMatch, accessToken)
	}
	return []client.Identity{}, fmt.Errorf("No auth provider configured")
}
