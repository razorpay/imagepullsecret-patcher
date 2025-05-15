package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

var (
	// Config
	configForce                bool          = true
	configDebug                bool          = false
	configManagedOnly          bool          = false
	configRunOnce              bool          = false
	configAllServiceAccount    bool          = true
	configDockerconfigjson     string        = ""
	configDockerConfigJSONPath string        = ""
	configSecretName           string        = "registry" // default to image-pull-secret
	configExcludedNamespaces   string        = ""
	configServiceAccounts      string        = defaultServiceAccountName
	configLoopDuration         time.Duration = 10 * time.Second
	// AWS ConfigMap configs
	configAWSConfigMapName      string = "aws-configs"
	configAWSConfigFilePath     string = "/config/aws-configs"

	dockerConfigJSON string
)

const (
	annotationImagepullsecretPatcherExclude = "k8s.titansoft.com/imagepullsecret-patcher-exclude"
)

type k8sClient struct {
	clientset kubernetes.Interface
}

func main() {
	// parse flags
	flag.BoolVar(&configForce, "force", LookUpEnvOrBool("CONFIG_FORCE", configForce), "force to overwrite secrets when not match")
	flag.BoolVar(&configDebug, "debug", LookUpEnvOrBool("CONFIG_DEBUG", configDebug), "show DEBUG logs")
	flag.BoolVar(&configManagedOnly, "managedonly", LookUpEnvOrBool("CONFIG_MANAGEDONLY", configManagedOnly), "only modify secrets which are annotated as managed by imagepullsecret")
	flag.BoolVar(&configRunOnce, "runonce", LookUpEnvOrBool("CONFIG_RUNONCE", configRunOnce), "run a single update and exit instead of looping")
	flag.BoolVar(&configAllServiceAccount, "allserviceaccount", LookUpEnvOrBool("CONFIG_ALLSERVICEACCOUNT", configAllServiceAccount), "if false, patch just default service account; if true, list and patch all service accounts")
	flag.StringVar(&configDockerconfigjson, "dockerconfigjson", LookupEnvOrString("CONFIG_DOCKERCONFIGJSON", configDockerconfigjson), "json credential for authenicating container registry, exclusive with `dockerconfigjsonpath`")
	flag.StringVar(&configDockerConfigJSONPath, "dockerconfigjsonpath", LookupEnvOrString("CONFIG_DOCKERCONFIGJSONPATH", configDockerConfigJSONPath), "path to json file containing credentials for the registry to be distributed, exclusive with `dockerconfigjson`")
	flag.StringVar(&configSecretName, "secretname", LookupEnvOrString("CONFIG_SECRETNAME", configSecretName), "set name of managed secrets")
	flag.StringVar(&configExcludedNamespaces, "excluded-namespaces", LookupEnvOrString("CONFIG_EXCLUDED_NAMESPACES", configExcludedNamespaces), "comma-separated namespaces excluded from processing")
	flag.StringVar(&configServiceAccounts, "serviceaccounts", LookupEnvOrString("CONFIG_SERVICEACCOUNTS", configServiceAccounts), "comma-separated list of serviceaccounts to patch")
	flag.DurationVar(&configLoopDuration, "loop-duration", LookupEnvOrDuration("CONFIG_LOOP_DURATION", configLoopDuration), "String defining the loop duration")
	
	// AWS ConfigMap flags
	flag.StringVar(&configAWSConfigMapName, "aws-configmap-name", LookupEnvOrString("CONFIG_AWS_CONFIGMAP_NAME", configAWSConfigMapName), "name of the AWS ConfigMap to be created")
	flag.StringVar(&configAWSConfigFilePath, "aws-config-file", LookupEnvOrString("CONFIG_AWS_CONFIG_FILE", configAWSConfigFilePath), "path to AWS config file to be included in the ConfigMap")
	
	flag.Parse()

	// setup logrus
	if configDebug {
		log.SetLevel(log.DebugLevel)
	}
	log.Info("Application started")

	// Validate input, as both of these being configured would have undefined behavior.
	if configDockerconfigjson != "" && configDockerConfigJSONPath != "" {
		log.Panic(fmt.Errorf("Cannot specify both `configdockerjson` and `configdockerjsonpath`"))
	}

	// create k8s clientset from in-cluster config
	config, err := rest.InClusterConfig()
	if err != nil {
		log.Panic(err)
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		log.Panic(err)
	}
	k8s := &k8sClient{
		clientset: clientset,
	}

	for {
		log.Debug("Loop started")
		loop(k8s)
		if configRunOnce {
			log.Info("Exiting after single loop per `CONFIG_RUNONCE`")
			os.Exit(0)
		}
		time.Sleep(configLoopDuration)
	}
}

func loop(k8s *k8sClient) {
	var err error

	// Populate secret value to set
	dockerConfigJSON, err = getDockerConfigJSON()
	if err != nil {
		log.Panic(err)
	}

	// get all namespaces
	namespaces, err := k8s.clientset.CoreV1().Namespaces().List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		log.Panic(err)
	}
	log.Debugf("Got %d namespaces", len(namespaces.Items))

	for _, ns := range namespaces.Items {
		namespace := ns.Name
		if namespaceIsExcluded(ns) {
			log.Infof("[%s] Namespace skipped", namespace)
			continue
		}
		log.Debugf("[%s] Start processing", namespace)
		
		// for each namespace, make sure the dockerconfig secret exists
		err = processSecret(k8s, namespace)
		if err != nil {
			// if has error in processing secret, should skip processing service account
			log.Error(err)
			continue
		}

		// for each namespace, make sure the AWS ConfigMap exists
		err = processAWSConfigMap(k8s, namespace)
		if err != nil {
			log.Error(err)
			continue
		}
		
		// get default service account, and patch image pull secret if not exist
		err = processServiceAccount(k8s, namespace)
		if err != nil {
			log.Error(err)
		}
	}
}

func namespaceIsExcluded(ns corev1.Namespace) bool {
	v, ok := ns.Annotations[annotationImagepullsecretPatcherExclude]
	if ok && v == "true" {
		return true
	}
	for _, ex := range strings.Split(configExcludedNamespaces, ",") {
		if ex == ns.Name {
			return true
		}
	}
	return false
}

func processSecret(k8s *k8sClient, namespace string) error {
	secret, err := k8s.clientset.CoreV1().Secrets(namespace).Get(context.TODO(), configSecretName, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		_, err := k8s.clientset.CoreV1().Secrets(namespace).Create(context.TODO(), dockerconfigSecret(namespace), metav1.CreateOptions{})
		if err != nil {
			return fmt.Errorf("[%s] Failed to create secret: %v", namespace, err)
		}
		log.Infof("[%s] Created secret", namespace)
	} else if err != nil {
		return fmt.Errorf("[%s] Failed to GET secret: %v", namespace, err)
	} else {
		if configManagedOnly && isManagedSecret(secret) {
			return fmt.Errorf("[%s] Secret is present but unmanaged", namespace)
		}
		switch verifySecret(secret) {
		case secretOk:
			log.Debugf("[%s] Secret is valid", namespace)
		case secretWrongType, secretNoKey, secretDataNotMatch:
			if configForce {
				log.Warnf("[%s] Secret is not valid, overwritting now", namespace)
				err = k8s.clientset.CoreV1().Secrets(namespace).Delete(context.TODO(), configSecretName, metav1.DeleteOptions{})
				if err != nil {
					return fmt.Errorf("[%s] Failed to delete secret [%s]: %v", namespace, configSecretName, err)
				}
				log.Warnf("[%s] Deleted secret [%s]", namespace, configSecretName)
				_, err = k8s.clientset.CoreV1().Secrets(namespace).Create(context.TODO(), dockerconfigSecret(namespace), metav1.CreateOptions{})
				if err != nil {
					return fmt.Errorf("[%s] Failed to create secret: %v", namespace, err)
				}
				log.Infof("[%s] Created secret", namespace)
			} else {
				return fmt.Errorf("[%s] Secret is not valid, set --force to true to overwrite", namespace)
			}
		}
	}
	return nil
}

func processServiceAccount(k8s *k8sClient, namespace string) error {
	sas, err := k8s.clientset.CoreV1().ServiceAccounts(namespace).List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("[%s] Failed to list service accounts: %v", namespace, err)
	}
	for _, sa := range sas.Items {
		if !configAllServiceAccount && stringNotInList(sa.Name, configServiceAccounts) {
			log.Debugf("[%s] Skip service account [%s]", namespace, sa.Name)
			continue
		}
		if includeImagePullSecret(&sa, configSecretName) {
			log.Debugf("[%s] ImagePullSecrets found", namespace)
			continue
		}
		patch, err := getPatchString(&sa, configSecretName)
		if err != nil {
			return fmt.Errorf("[%s] Failed to get patch string: %v", namespace, err)
		}
		_, err = k8s.clientset.CoreV1().ServiceAccounts(namespace).Patch(context.TODO(), sa.Name, types.StrategicMergePatchType, patch, metav1.PatchOptions{})
		if err != nil {
			return fmt.Errorf("[%s] Failed to patch imagePullSecrets to service account [%s]: %v", namespace, sa.Name, err)
		}
		log.Infof("[%s] Patched imagePullSecrets to service account [%s]", namespace, sa.Name)
	}
	return nil
}

func stringNotInList(a string, list string) bool {
	for _, b := range strings.Split(list, ",") {
		if b == a {
			return false
		}
	}
	return true
}

// awsConfigMap creates a ConfigMap with values parsed from an environment file
func awsConfigMap(namespace string) (*corev1.ConfigMap, error) {
	// Check if the config file exists
	fileInfo, err := os.Stat(configAWSConfigFilePath)
	if err != nil {
		return nil, fmt.Errorf("failed to access AWS config file: %v", err)
	}

	// If it's a directory, throw an error
	if fileInfo.IsDir() {
		return nil, fmt.Errorf("AWS config path is a directory, expected a file: %s", configAWSConfigFilePath)
	}

	// Read the content of the file
	content, err := os.ReadFile(configAWSConfigFilePath)
	if err != nil {
		return nil, fmt.Errorf("failed to read AWS config file: %v", err)
	}

	// Parse the environment file (key=value lines)
	data := make(map[string]string)
	lines := strings.Split(string(content), "\n")
	
	for _, line := range lines {
		// Skip empty lines or comment lines
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		
		// Split by first equals sign
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			log.Warnf("Ignoring invalid line in env file: %s", line)
			continue
		}
		
		key := strings.TrimSpace(parts[0])
		value := strings.TrimSpace(parts[1])
		
		// Remove quotes if present
		if len(value) > 1 && (strings.HasPrefix(value, "\"") && strings.HasSuffix(value, "\"")) || 
		   (strings.HasPrefix(value, "'") && strings.HasSuffix(value, "'")) {
			value = value[1 : len(value)-1]
		}
		
		data[key] = value
	}

	// Return error if no valid data was found
	if len(data) == 0 {
		return nil, fmt.Errorf("no valid entries found in environment file %s", configAWSConfigFilePath)
	}

	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      configAWSConfigMapName,
			Namespace: namespace,
			Annotations: map[string]string{
				annotationManagedBy: annotationAppName,
			},
		},
		Data: data,
	}, nil
}

// processAWSConfigMap ensures the AWS ConfigMap exists in the given namespace
func processAWSConfigMap(k8s *k8sClient, namespace string) error {
	configMap, err := k8s.clientset.CoreV1().ConfigMaps(namespace).Get(context.TODO(), configAWSConfigMapName, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		// Create the AWS ConfigMap from the file
		awsConfigMapObj, err := awsConfigMap(namespace)
		if err != nil {
			// If the file doesn't exist or is inaccessible, log it and return without error
			log.Debugf("[%s] Skipping AWS ConfigMap creation: %v", namespace, err)
			return nil
		}
		
		_, err = k8s.clientset.CoreV1().ConfigMaps(namespace).Create(context.TODO(), awsConfigMapObj, metav1.CreateOptions{})
		if err != nil {
			return fmt.Errorf("[%s] Failed to create AWS ConfigMap: %v", namespace, err)
		}
		log.Infof("[%s] Created AWS ConfigMap", namespace)
	} else if err != nil {
		return fmt.Errorf("[%s] Failed to GET AWS ConfigMap: %v", namespace, err)
	} else {
		// Check if the ConfigMap is managed by us
		if configManagedOnly && !isManagedConfigMap(configMap) {
			return fmt.Errorf("[%s] AWS ConfigMap is present but unmanaged", namespace)
		}
		
		// Read the current AWS config file
		awsConfigMapObj, err := awsConfigMap(namespace)
		if err != nil {
			// If the file doesn't exist anymore, consider removing the ConfigMap
			log.Warnf("[%s] AWS config file is no longer accessible: %v", namespace, err)
			if configForce {
				log.Warnf("[%s] Deleting AWS ConfigMap since config file is gone", namespace)
				err = k8s.clientset.CoreV1().ConfigMaps(namespace).Delete(context.TODO(), configAWSConfigMapName, metav1.DeleteOptions{})
				if err != nil {
					return fmt.Errorf("[%s] Failed to delete AWS ConfigMap [%s]: %v", namespace, configAWSConfigMapName, err)
				}
				log.Infof("[%s] Deleted AWS ConfigMap", namespace)
			}
			return nil
		}
		
		// Check if the ConfigMap data matches what we read from the file
		if !mapsEqual(configMap.Data, awsConfigMapObj.Data) {
			if configForce {
				log.Warnf("[%s] AWS ConfigMap is not valid, overwriting now", namespace)
				err = k8s.clientset.CoreV1().ConfigMaps(namespace).Delete(context.TODO(), configAWSConfigMapName, metav1.DeleteOptions{})
				if err != nil {
					return fmt.Errorf("[%s] Failed to delete AWS ConfigMap [%s]: %v", namespace, configAWSConfigMapName, err)
				}
				log.Warnf("[%s] Deleted AWS ConfigMap [%s]", namespace, configAWSConfigMapName)
				_, err = k8s.clientset.CoreV1().ConfigMaps(namespace).Create(context.TODO(), awsConfigMapObj, metav1.CreateOptions{})
				if err != nil {
					return fmt.Errorf("[%s] Failed to create AWS ConfigMap: %v", namespace, err)
				}
				log.Infof("[%s] Created AWS ConfigMap", namespace)
			} else {
				return fmt.Errorf("[%s] AWS ConfigMap is not valid, set --force to true to overwrite", namespace)
			}
		} else {
			log.Debugf("[%s] AWS ConfigMap is valid", namespace)
		}
	}
	return nil
}

// isManagedConfigMap checks if the ConfigMap is managed by this application
func isManagedConfigMap(configMap *corev1.ConfigMap) bool {
	if k, ok := configMap.ObjectMeta.Annotations[annotationManagedBy]; ok {
		if k == annotationAppName {
			return true
		}
	}
	return false
}

// mapsEqual compares two string maps for equality
func mapsEqual(map1, map2 map[string]string) bool {
	if len(map1) != len(map2) {
		return false
	}
	
	for k, v1 := range map1 {
		if v2, ok := map2[k]; !ok || v1 != v2 {
			return false
		}
	}
	
	return true
}
