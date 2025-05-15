package main

import (
	"context"
	"fmt"
	"io/ioutil"
	"os"
	"testing"

	"github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

var testCasesProcessSecret = []testCase{
	{
		name: "no secret",
		prepSteps: []step{
			assertNoSecret,
		},
		testSteps: []step{
			processSecretDefault,
			assertSecretIsValid,
		},
	},
	{
		name: "has valid secret",
		prepSteps: []step{
			helperCreateValidSecret,
			assertSecretIsValid,
		},
		testSteps: []step{
			processSecretDefault,
			assertSecretIsValid,
		},
	},
	{
		name: "has invalid secret - force on",
		prepSteps: []step{
			helperForceOn,
			helperCreateOpaqueSecret,
			assertSecretIsInvalid,
		},
		testSteps: []step{
			processSecretDefault,
			assertSecretIsValid,
		},
	},
	{
		name: "has invalid secret - force off",
		prepSteps: []step{
			helperForceOff,
			helperCreateOpaqueSecret,
			assertSecretIsInvalid,
		},
		testSteps: []step{
			assertHasError(processSecretDefault),
			assertSecretIsInvalid,
		},
	},
}

var testCasesProcessServiceAccount = []testCase{
	{
		name: "no image pull secret",
		prepSteps: []step{
			helperCreateServiceAccountWithoutImagePullSecret(defaultServiceAccountName),
			assertHasError(assertHasImagePullSecret(configSecretName, defaultServiceAccountName)),
		},
		testSteps: []step{
			processServiceAccountDefault,
			assertHasImagePullSecret(configSecretName, defaultServiceAccountName),
		},
	},
	{
		name: "has same image pull secret",
		prepSteps: []step{
			helperCreateServiceAccountWithImagePullSecret(configSecretName, defaultServiceAccountName),
			assertHasImagePullSecret(configSecretName, defaultServiceAccountName),
		},
		testSteps: []step{
			processServiceAccountDefault,
			assertHasImagePullSecret(configSecretName, defaultServiceAccountName),
		},
	},
	{
		name: "has different image pull secret",
		prepSteps: []step{
			helperCreateServiceAccountWithImagePullSecret("other-secret", defaultServiceAccountName),
			assertHasImagePullSecret("other-secret", defaultServiceAccountName),
			assertHasError(assertHasImagePullSecret(configSecretName, defaultServiceAccountName)),
		},
		testSteps: []step{
			processServiceAccountDefault,
			assertHasImagePullSecret("other-secret", defaultServiceAccountName),
			assertHasImagePullSecret(configSecretName, defaultServiceAccountName),
		},
	},
	{
		name: "non-default service account - skip when allServiceAccount off",
		prepSteps: []step{
			helperAllServiceAccountOff,
			helperCreateServiceAccountWithoutImagePullSecret("other-service-account"),
			assertHasError(assertHasImagePullSecret(configSecretName, "other-service-account")),
		},
		testSteps: []step{
			processServiceAccountDefault,
			assertHasError(assertHasImagePullSecret(configSecretName, "other-service-account")),
		},
	},
	{
		name: "non-default service account - patch when allServiceAccount on",
		prepSteps: []step{
			helperAllServiceAccountOn,
			helperCreateServiceAccountWithoutImagePullSecret("other-service-account"),
			assertHasError(assertHasImagePullSecret(configSecretName, "other-service-account")),
		},
		testSteps: []step{
			processServiceAccountDefault,
			assertHasImagePullSecret(configSecretName, "other-service-account"),
		},
	},
}

func TestProcessSecret(t *testing.T) {
	for _, tc := range testCasesProcessSecret {
		runTestCase(t, "ProcessSecret", tc)
	}
}

func TestProcessServiceAccount(t *testing.T) {
	for _, tc := range testCasesProcessServiceAccount {
		runTestCase(t, "ProcessServiceAccount", tc)
	}
}

// TestMapsEqual tests the map comparison function
func TestMapsEqual(t *testing.T) {
	// Test cases
	testCases := []struct {
		name   string
		map1   map[string]string
		map2   map[string]string
		equal  bool
	}{
		{
			name: "identical maps",
			map1: map[string]string{
				"AWS_REGION":      "us-west-2",
				"AWS_SQS_ENDPOINT": "https://sqs.us-west-2.amazonaws.com",
			},
			map2: map[string]string{
				"AWS_REGION":      "us-west-2",
				"AWS_SQS_ENDPOINT": "https://sqs.us-west-2.amazonaws.com",
			},
			equal: true,
		},
		{
			name: "different values",
			map1: map[string]string{
				"AWS_REGION":      "us-west-2",
				"AWS_SQS_ENDPOINT": "https://sqs.us-west-2.amazonaws.com",
			},
			map2: map[string]string{
				"AWS_REGION":      "us-east-1",
				"AWS_SQS_ENDPOINT": "https://sqs.us-west-2.amazonaws.com",
			},
			equal: false,
		},
		{
			name: "different keys",
			map1: map[string]string{
				"AWS_REGION":      "us-west-2",
				"AWS_SQS_ENDPOINT": "https://sqs.us-west-2.amazonaws.com",
			},
			map2: map[string]string{
				"AWS_REGION":      "us-west-2",
				"AWS_SNS_ENDPOINT": "https://sns.us-west-2.amazonaws.com",
			},
			equal: false,
		},
		{
			name: "different lengths",
			map1: map[string]string{
				"AWS_REGION":      "us-west-2",
				"AWS_SQS_ENDPOINT": "https://sqs.us-west-2.amazonaws.com",
				"AWS_ACCOUNT_ID":  "123456789012",
			},
			map2: map[string]string{
				"AWS_REGION":      "us-west-2",
				"AWS_SQS_ENDPOINT": "https://sqs.us-west-2.amazonaws.com",
			},
			equal: false,
		},
	}
	
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			result := mapsEqual(tc.map1, tc.map2)
			if result != tc.equal {
				t.Errorf("mapsEqual() = %v, want %v", result, tc.equal)
			}
		})
	}
}

type step func(*k8sClient) error

type testCase struct {
	name      string // name of the test
	prepSteps []step // preparation steps
	testSteps []step // test steps
}

func runTestCase(t *testing.T, testName string, tc testCase) {
	// disable logrus
	logrus.SetOutput(ioutil.Discard)

	// create fake client
	k8s := &k8sClient{
		clientset: fake.NewSimpleClientset(),
	}

	// run preparation steps
	for _, step := range tc.prepSteps {
		if err := step(k8s); err != nil {
			t.Errorf("%s(%s) failed during preparation: %v", testName, tc.name, err)
			return
		}
	}

	// run through test steps
	for _, step := range tc.testSteps {
		if err := step(k8s); err != nil {
			t.Errorf("%s(%s) failed during test: %v", testName, tc.name, err)
			return
		}
	}
}

func processSecretDefault(k8s *k8sClient) error {
	return processSecret(k8s, v1.NamespaceDefault)
}

func processServiceAccountDefault(k8s *k8sClient) error {
	return processServiceAccount(k8s, v1.NamespaceDefault)
}

func TestNamespaceIsExcluded(t *testing.T) {
	for _, tc := range []struct {
		name      string
		config    string
		namespace corev1.Namespace
		expected  bool
	}{
		{
			name:   "empty config",
			config: "",
			namespace: corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: "kube-system",
				},
			},
			expected: false,
		},
		{
			name:   "appear in config",
			config: "kube-system,other-namespace",
			namespace: corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: "kube-system",
				},
			},
			expected: true,
		},
		{
			name:   "not appear in config",
			config: "default,other-namespace",
			namespace: corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: "kube-system",
				},
			},
			expected: false,
		},
		{
			name:   "namespace has annotation true",
			config: "",
			namespace: corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: "kube-system",
					Annotations: map[string]string{
						"k8s.titansoft.com/imagepullsecret-patcher-exclude": "true",
					},
				},
			},
			expected: true,
		},
	} {
		configExcludedNamespaces = tc.config
		if actual := namespaceIsExcluded(tc.namespace); actual != tc.expected {
			t.Errorf("TestNamespaceIsExcluded(%s) failed: expected %v, got %v", tc.name, tc.expected, actual)
		}
	}
}

// a set of helper functions
func helperCreateValidSecret(k8s *k8sClient) error {
	_, err := k8s.clientset.CoreV1().Secrets(v1.NamespaceDefault).Create(context.TODO(), dockerconfigSecret(v1.NamespaceDefault), metav1.CreateOptions{})
	return err
}

func helperCreateOpaqueSecret(k8s *k8sClient) error {
	_, err := k8s.clientset.CoreV1().Secrets(v1.NamespaceDefault).Create(context.TODO(), &v1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      configSecretName,
			Namespace: v1.NamespaceDefault,
		},
		Type: corev1.SecretTypeOpaque,
	}, metav1.CreateOptions{})
	return err
}

func helperCreateServiceAccountWithoutImagePullSecret(serviceAccountName string) step {
	return func(k8s *k8sClient) error {
		_, err := k8s.clientset.CoreV1().ServiceAccounts(v1.NamespaceDefault).Create(context.TODO(), &v1.ServiceAccount{
			ObjectMeta: metav1.ObjectMeta{
				Name:      serviceAccountName,
				Namespace: v1.NamespaceDefault,
			},
		}, metav1.CreateOptions{})
		return err
	}
}

func helperCreateServiceAccountWithImagePullSecret(secretName, serviceAccountName string) step {
	return func(k8s *k8sClient) error {
		_, err := k8s.clientset.CoreV1().ServiceAccounts(v1.NamespaceDefault).Create(context.TODO(), &v1.ServiceAccount{
			ObjectMeta: metav1.ObjectMeta{
				Name:      serviceAccountName,
				Namespace: v1.NamespaceDefault,
			},
			ImagePullSecrets: []v1.LocalObjectReference{
				{
					Name: secretName,
				},
			},
		}, metav1.CreateOptions{})
		return err
	}
}

func helperForceOn(_ *k8sClient) error {
	configForce = true
	return nil
}

func helperForceOff(_ *k8sClient) error {
	configForce = false
	return nil
}

func helperAllServiceAccountOn(_ *k8sClient) error {
	configAllServiceAccount = true
	return nil
}

func helperAllServiceAccountOff(_ *k8sClient) error {
	configAllServiceAccount = false
	return nil
}

// a set of assertion functions
func assertNoSecret(k8s *k8sClient) error {
	_, err := k8s.clientset.CoreV1().Secrets(v1.NamespaceDefault).Get(context.TODO(), configSecretName, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		return nil
	}
	if err == nil {
		return fmt.Errorf("assert no secret but found")
	}
	return err
}

func assertSecretIsValid(k8s *k8sClient) error {
	secret, err := k8s.clientset.CoreV1().Secrets(v1.NamespaceDefault).Get(context.TODO(), configSecretName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("assert secret valid but no found")
	}
	if result := verifySecret(secret); result != secretOk {
		return fmt.Errorf("assert secret valid but invalid: %v", result)
	}
	return nil
}

func assertSecretIsInvalid(k8s *k8sClient) error {
	secret, err := k8s.clientset.CoreV1().Secrets(v1.NamespaceDefault).Get(context.TODO(), configSecretName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("assert secret invalid but no found")
	}
	if result := verifySecret(secret); result == secretOk {
		return fmt.Errorf("assert secret invalid but valid")
	}
	return nil
}

func assertHasError(fn step) step {
	return func(k8s *k8sClient) error {
		if err := fn(k8s); err == nil {
			return fmt.Errorf("assert has error but not")
		}
		return nil
	}
}

func assertHasImagePullSecret(secretName, serviceAccountName string) step {
	return func(k8s *k8sClient) error {
		sa, err := k8s.clientset.CoreV1().ServiceAccounts(v1.NamespaceDefault).Get(context.TODO(), serviceAccountName, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if includeImagePullSecret(sa, secretName) {
			return nil
		}
		return fmt.Errorf("assert has image pull secret [%s] but not found", secretName)
	}
}

// TestAWSConfigMap tests the AWS ConfigMap creation from an environment file
func TestAWSConfigMap(t *testing.T) {
	// Create a temporary file
	tempFile, err := os.CreateTemp("", "aws-config-test")
	if err != nil {
		t.Fatalf("Failed to create temp file: %v", err)
	}
	defer os.Remove(tempFile.Name())
	
	// Set the config path to our temp file
	configAWSConfigFilePath = tempFile.Name()
	
	// Create test content with various formats
	testContent := `
# This is a comment
AWS_REGION=us-west-2
  AWS_SQS_ENDPOINT = https://sqs.us-west-2.amazonaws.com  
AWS_SNS_ENDPOINT="https://sns.us-west-2.amazonaws.com"
AWS_ACCOUNT_ID = '123456789012'

# Empty line above
INVALID_LINE
`
	
	// Write the content to the file
	if _, err := tempFile.WriteString(testContent); err != nil {
		t.Fatalf("Failed to write test content to file: %v", err)
	}
	
	// Close the file to ensure content is flushed
	tempFile.Close()
	
	// Call the function
	configMap, err := awsConfigMap("default")
	if err != nil {
		t.Fatalf("awsConfigMap returned an error: %v", err)
	}
	
	// Check that the ConfigMap data has the expected key-value pairs
	expectedData := map[string]string{
		"AWS_REGION":      "us-west-2",
		"AWS_SQS_ENDPOINT": "https://sqs.us-west-2.amazonaws.com",
		"AWS_SNS_ENDPOINT": "https://sns.us-west-2.amazonaws.com",
		"AWS_ACCOUNT_ID":  "123456789012",
	}
	
	if !mapsEqual(configMap.Data, expectedData) {
		t.Errorf("ConfigMap data does not match expected. Got %v, want %v", configMap.Data, expectedData)
	}
	
	// Check the metadata
	if configMap.Name != configAWSConfigMapName {
		t.Errorf("ConfigMap name is %s, want %s", configMap.Name, configAWSConfigMapName)
	}
	
	if configMap.Namespace != "default" {
		t.Errorf("ConfigMap namespace is %s, want default", configMap.Namespace)
	}
	
	// Test with file containing only comments and empty lines
	tempFile2, err := os.CreateTemp("", "aws-config-test2")
	if err != nil {
		t.Fatalf("Failed to create temp file: %v", err)
	}
	defer os.Remove(tempFile2.Name())
	
	invalidContent := `
# Just a comment
   
# Another comment
`
	if _, err := tempFile2.WriteString(invalidContent); err != nil {
		t.Fatalf("Failed to write test content to file: %v", err)
	}
	tempFile2.Close()
	
	configAWSConfigFilePath = tempFile2.Name()
	_, err = awsConfigMap("default")
	if err == nil {
		t.Errorf("Expected error for file with no valid entries, got nil")
	}
	
	// Test with nonexistent file
	os.Remove(tempFile.Name())
	configAWSConfigFilePath = tempFile.Name()
	
	_, err = awsConfigMap("default")
	if err == nil {
		t.Errorf("Expected error when file doesn't exist, got nil")
	}
}
