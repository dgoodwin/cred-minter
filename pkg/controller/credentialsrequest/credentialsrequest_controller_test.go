/*
Copyright 2018 The OpenShift Authors.

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

package credentialsrequest

import (
	"context"
	"encoding/base64"
	"fmt"
	"testing"

	"github.com/golang/mock/gomock"
	log "github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/openshift/cred-minter/pkg/apis"
	minterv1 "github.com/openshift/cred-minter/pkg/apis/credminter/v1beta1"
	minteraws "github.com/openshift/cred-minter/pkg/aws"
	"github.com/openshift/cred-minter/pkg/aws/actuator"
	mockaws "github.com/openshift/cred-minter/pkg/aws/mock"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/service/iam"
)

var c client.Client

func init() {
	log.SetLevel(log.DebugLevel)
}

func TestCredentialsRequestReconcile(t *testing.T) {
	apis.AddToScheme(scheme.Scheme)

	// Utility function to get the test credentials request from the fake client
	getCR := func(c client.Client) *minterv1.CredentialsRequest {
		cr := &minterv1.CredentialsRequest{}
		err := c.Get(context.TODO(), client.ObjectKey{Name: testCRName, Namespace: testNamespace}, cr)
		if err == nil {
			return cr
		}
		return nil
	}

	getSecret := func(c client.Client) *corev1.Secret {
		secret := &corev1.Secret{}
		err := c.Get(context.TODO(), client.ObjectKey{Name: testSecretName, Namespace: testSecretNamespace}, secret)
		if err == nil {
			return secret
		}
		return nil
	}

	tests := []struct {
		name               string
		existing           []runtime.Object
		expectErr          bool
		buildMockAWSClient func(mockCtrl *gomock.Controller) *mockaws.MockClient
		validate           func(client.Client, *testing.T)
	}{
		{
			name: "add finalizer",
			existing: []runtime.Object{
				createTestNamespace(testSecretNamespace),
				func() *minterv1.CredentialsRequest {
					cr := testCredentialsRequest(t)
					// Remove the finalizer
					cr.ObjectMeta.Finalizers = []string{}
					return cr
				}(),
				testAWSCredsSecret("kube-system", "aws-creds", "akeyid", "secretaccess"),
			},
			buildMockAWSClient: func(mockCtrl *gomock.Controller) *mockaws.MockClient {
				mockAWSClient := mockaws.NewMockClient(mockCtrl)
				return mockAWSClient
			},
			validate: func(c client.Client, t *testing.T) {
				cr := getCR(c)
				if cr == nil || !HasFinalizer(cr, minterv1.FinalizerDeprovision) {
					t.Errorf("did not get expected finalizer")
				}
				assert.False(t, cr.Status.Provisioned)
			},
		},
		{
			name: "new credential",
			existing: []runtime.Object{
				createTestNamespace(testSecretNamespace),
				testCredentialsRequest(t),
				testAWSCredsSecret("kube-system", "aws-creds", "akeyid", "secretaccess"),
			},
			buildMockAWSClient: func(mockCtrl *gomock.Controller) *mockaws.MockClient {
				mockAWSClient := mockaws.NewMockClient(mockCtrl)
				mockGetUserNotFound(mockAWSClient)
				mockPutUserPolicy(mockAWSClient)
				mockCreateUser(mockAWSClient)
				mockListAccessKeysEmpty(mockAWSClient)
				mockCreateAccessKey(mockAWSClient, testAWSAccessKeyID, testAWSSecretAccessKey)
				return mockAWSClient
			},
			validate: func(c client.Client, t *testing.T) {
				targetSecret := getSecret(c)
				if assert.NotNil(t, targetSecret) {
					assert.Equal(t, testAWSAccessKeyID,
						base64DecodeOrFail(t, targetSecret.Data["aws_access_key_id"]))
					assert.Equal(t, testAWSSecretAccessKey,
						base64DecodeOrFail(t, targetSecret.Data["aws_secret_access_key"]))
				}
				cr := getCR(c)
				assert.True(t, cr.Status.Provisioned)
			},
		},
		{
			name: "cred exists",
			existing: []runtime.Object{
				createTestNamespace(testSecretNamespace),
				testCredentialsRequest(t),
				testAWSCredsSecret("kube-system", "aws-creds", "akeyid", "secretaccess"),
				testAWSCredsSecret(testNamespace, testSecretName, testAWSAccessKeyID, testAWSSecretAccessKey),
			},
			buildMockAWSClient: func(mockCtrl *gomock.Controller) *mockaws.MockClient {
				mockAWSClient := mockaws.NewMockClient(mockCtrl)
				mockGetUser(mockAWSClient)
				mockPutUserPolicy(mockAWSClient)
				mockListAccessKeys(mockAWSClient, testAWSAccessKeyID)
				return mockAWSClient
			},
			validate: func(c client.Client, t *testing.T) {
				targetSecret := getSecret(c)
				if assert.NotNil(t, targetSecret) {
					assert.Equal(t, testAWSAccessKeyID,
						base64DecodeOrFail(t, targetSecret.Data["aws_access_key_id"]))
					assert.Equal(t, testAWSSecretAccessKey,
						base64DecodeOrFail(t, targetSecret.Data["aws_secret_access_key"]))
				}
				cr := getCR(c)
				assert.True(t, cr.Status.Provisioned)
			},
		},
		{
			name: "cred missing access key exists",
			existing: []runtime.Object{
				createTestNamespace(testSecretNamespace),
				testCredentialsRequest(t),
				testAWSCredsSecret("kube-system", "aws-creds", "akeyid", "secretaccess"),
			},
			buildMockAWSClient: func(mockCtrl *gomock.Controller) *mockaws.MockClient {
				mockAWSClient := mockaws.NewMockClient(mockCtrl)
				mockGetUser(mockAWSClient)
				mockPutUserPolicy(mockAWSClient)
				mockListAccessKeys(mockAWSClient, testAWSAccessKeyID)
				mockCreateAccessKey(mockAWSClient, testAWSAccessKeyID2, testAWSSecretAccessKey2)
				mockDeleteAccessKey(mockAWSClient, testAWSAccessKeyID)
				return mockAWSClient
			},
			validate: func(c client.Client, t *testing.T) {
				targetSecret := getSecret(c)
				if assert.NotNil(t, targetSecret) {
					assert.Equal(t, testAWSAccessKeyID2,
						base64DecodeOrFail(t, targetSecret.Data["aws_access_key_id"]))
					assert.Equal(t, testAWSSecretAccessKey2,
						base64DecodeOrFail(t, targetSecret.Data["aws_secret_access_key"]))
					assert.Equal(t, fmt.Sprintf("%s/%s", testNamespace, testCRName), targetSecret.Annotations[minterv1.AnnotationCredentialsRequest])
				}
				cr := getCR(c)
				assert.True(t, cr.Status.Provisioned)
			},
		},
		{
			name: "cred exists access key missing",
			existing: []runtime.Object{
				createTestNamespace(testSecretNamespace),
				testCredentialsRequest(t),
				testAWSCredsSecret("kube-system", "aws-creds", "akeyid", "secretaccess"),
				testAWSCredsSecret(testNamespace, testSecretName, testAWSAccessKeyID, testAWSSecretAccessKey),
			},
			buildMockAWSClient: func(mockCtrl *gomock.Controller) *mockaws.MockClient {
				mockAWSClient := mockaws.NewMockClient(mockCtrl)
				mockGetUser(mockAWSClient)
				mockPutUserPolicy(mockAWSClient)
				mockListAccessKeysEmpty(mockAWSClient)
				mockCreateAccessKey(mockAWSClient, testAWSAccessKeyID2, testAWSSecretAccessKey2)
				return mockAWSClient
			},
			validate: func(c client.Client, t *testing.T) {
				targetSecret := getSecret(c)
				if assert.NotNil(t, targetSecret) {
					assert.Equal(t, testAWSAccessKeyID2,
						base64DecodeOrFail(t, targetSecret.Data["aws_access_key_id"]))
					assert.Equal(t, testAWSSecretAccessKey2,
						base64DecodeOrFail(t, targetSecret.Data["aws_secret_access_key"]))
					assert.Equal(t, fmt.Sprintf("%s/%s", testNamespace, testCRName), targetSecret.Annotations[minterv1.AnnotationCredentialsRequest])
				}
				cr := getCR(c)
				assert.True(t, cr.Status.Provisioned)
			},
		},
		{
			name: "cred deletion",
			existing: []runtime.Object{
				createTestNamespace(testSecretNamespace),
				testCredentialsRequestWithDeletionTimestamp(t),
				testAWSCredsSecret("kube-system", "aws-creds", "akeyid", "secretaccess"),
				testAWSCredsSecret(testNamespace, testSecretName, testAWSAccessKeyID, testAWSSecretAccessKey),
			},
			buildMockAWSClient: func(mockCtrl *gomock.Controller) *mockaws.MockClient {
				mockAWSClient := mockaws.NewMockClient(mockCtrl)
				mockListAccessKeys(mockAWSClient, testAWSAccessKeyID)
				mockDeleteUser(mockAWSClient)
				mockDeleteUserPolicy(mockAWSClient)
				mockDeleteAccessKey(mockAWSClient, testAWSAccessKeyID)
				return mockAWSClient
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			mockCtrl := gomock.NewController(t)
			defer mockCtrl.Finish()

			mockAWSClient := test.buildMockAWSClient(mockCtrl)
			fakeClient := fake.NewFakeClient(test.existing...)
			codec, err := minterv1.NewCodec()
			if err != nil {
				t.Errorf("error creating codec: %v", err)
				t.FailNow()
				return
			}
			rcr := &ReconcileCredentialsRequest{
				Client: fakeClient,
				Actuator: &actuator.AWSActuator{
					Client: fakeClient,
					Codec:  codec,
					Scheme: scheme.Scheme,
					AWSClientBuilder: func(accessKeyID, secretAccessKey []byte) (minteraws.Client, error) {
						return mockAWSClient, nil
					},
				},
			}

			_, err = rcr.Reconcile(reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      testCRName,
					Namespace: testNamespace,
				},
			})

			if test.validate != nil {
				test.validate(fakeClient, t)
			}

			if err != nil && !test.expectErr {
				t.Errorf("Unexpected error: %v", err)
			}
			if err == nil && test.expectErr {
				t.Errorf("Expected error but got none")
			}
		})
	}
}

const (
	testCRName              = "openshift-component-a"
	testNamespace           = "myproject"
	testClusterName         = "testcluster"
	testClusterID           = "e415fe1c-f894-11e8-8eb2-f2801f1b9fd1"
	testSecretName          = "test-secret"
	testSecretNamespace     = "myproject"
	testAWSUser             = "mycluster-test-aws-user"
	testAWSUserID           = "FAKEAWSUSERID"
	testAWSAccessKeyID      = "FAKEAWSACCESSKEYID"
	testAWSAccessKeyID2     = "FAKEAWSACCESSKEYID2"
	testAWSSecretAccessKey  = "KEEPITSECRET"
	testAWSSecretAccessKey2 = "KEEPITSECRET2"
)

func testCredentialsRequestWithDeletionTimestamp(t *testing.T) *minterv1.CredentialsRequest {
	cr := testCredentialsRequest(t)
	now := metav1.Now()
	cr.DeletionTimestamp = &now
	return cr
}

func testCredentialsRequest(t *testing.T) *minterv1.CredentialsRequest {
	codec, err := minterv1.NewCodec()
	if err != nil {
		t.Logf("error creating new codec: %v", err)
		t.FailNow()
		return nil
	}
	awsProvSpec, err := codec.EncodeProviderSpec(
		&minterv1.AWSProviderSpec{
			StatementEntries: []minterv1.StatementEntry{
				{
					Effect: "Allow",
					Action: []string{
						"s3:CreateBucket",
						"s3:DeleteBucket",
					},
					Resource: "*",
				},
			},
		})
	if err != nil {
		t.Logf("error encoding: %v", err)
		t.FailNow()
		return nil
	}
	awsStatus, err := codec.EncodeProviderStatus(
		&minterv1.AWSProviderStatus{
			User: testAWSUser,
		})
	if err != nil {
		t.Logf("error encoding: %v", err)
		t.FailNow()
		return nil
	}
	return &minterv1.CredentialsRequest{
		ObjectMeta: metav1.ObjectMeta{
			Name:        testCRName,
			Namespace:   testNamespace,
			Finalizers:  []string{minterv1.FinalizerDeprovision},
			UID:         types.UID("1234"),
			Annotations: map[string]string{},
		},
		Spec: minterv1.CredentialsRequestSpec{
			ClusterName:  testClusterName,
			ClusterID:    testClusterID,
			SecretRef:    corev1.ObjectReference{Name: testSecretName, Namespace: testSecretNamespace},
			ProviderSpec: awsProvSpec,
		},
		Status: minterv1.CredentialsRequestStatus{
			ProviderStatus: awsStatus,
		},
	}
}

func createTestNamespace(namespace string) *corev1.Namespace {
	return &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: namespace,
		},
	}
}

func testAWSCredsSecret(namespace, name, accessKeyID, secretAccessKey string) *corev1.Secret {
	s := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Data: map[string][]byte{
			"aws_access_key_id":     []byte(base64.StdEncoding.EncodeToString([]byte(accessKeyID))),
			"aws_secret_access_key": []byte(base64.StdEncoding.EncodeToString([]byte(secretAccessKey))),
		},
	}
	return s
}

func mockGetUserNotFound(mockAWSClient *mockaws.MockClient) {
	mockAWSClient.EXPECT().GetUser(gomock.Any()).Return(nil, awserr.New(iam.ErrCodeNoSuchEntityException, "no such entity", nil))
}

func mockGetUser(mockAWSClient *mockaws.MockClient) {
	mockAWSClient.EXPECT().GetUser(gomock.Any()).Return(
		&iam.GetUserOutput{
			User: &iam.User{
				UserId:   aws.String(testAWSUserID),
				UserName: aws.String(testAWSUser),
				Tags: []*iam.Tag{
					{
						Key:   aws.String("tectonicClusterID"),
						Value: aws.String("testClusterID"),
					},
				},
			},
		}, nil)
}

func mockDeleteUser(mockAWSClient *mockaws.MockClient) {
	mockAWSClient.EXPECT().DeleteUser(gomock.Any()).Return(
		&iam.DeleteUserOutput{}, nil)
}

func mockDeleteUserPolicy(mockAWSClient *mockaws.MockClient) {
	mockAWSClient.EXPECT().DeleteUserPolicy(gomock.Any()).Return(
		&iam.DeleteUserPolicyOutput{}, nil)
}

func mockListAccessKeysEmpty(mockAWSClient *mockaws.MockClient) {
	mockAWSClient.EXPECT().ListAccessKeys(
		&iam.ListAccessKeysInput{
			UserName: aws.String(testAWSUser),
		}).Return(
		&iam.ListAccessKeysOutput{
			AccessKeyMetadata: []*iam.AccessKeyMetadata{},
		}, nil)
}

func mockListAccessKeys(mockAWSClient *mockaws.MockClient, accessKeyID string) {
	mockAWSClient.EXPECT().ListAccessKeys(
		&iam.ListAccessKeysInput{
			UserName: aws.String(testAWSUser),
		}).Return(
		&iam.ListAccessKeysOutput{
			AccessKeyMetadata: []*iam.AccessKeyMetadata{
				{
					AccessKeyId: aws.String(accessKeyID),
				},
			},
		}, nil)
}

func mockCreateUser(mockAWSClient *mockaws.MockClient) {
	mockAWSClient.EXPECT().CreateUser(
		&iam.CreateUserInput{
			UserName: aws.String(testAWSUser),
			// TODO: tags?
		}).Return(
		&iam.CreateUserOutput{
			User: &iam.User{
				UserName: aws.String(testAWSUser),
				UserId:   aws.String(testAWSUserID),
			},
		}, nil)
}

func mockCreateAccessKey(mockAWSClient *mockaws.MockClient, accessKeyID, secretAccessKey string) {
	mockAWSClient.EXPECT().CreateAccessKey(
		&iam.CreateAccessKeyInput{
			UserName: aws.String(testAWSUser),
		}).Return(
		&iam.CreateAccessKeyOutput{
			AccessKey: &iam.AccessKey{
				AccessKeyId:     aws.String(accessKeyID),
				SecretAccessKey: aws.String(secretAccessKey),
			},
		}, nil)
}

func mockDeleteAccessKey(mockAWSClient *mockaws.MockClient, accessKeyID string) {
	mockAWSClient.EXPECT().DeleteAccessKey(
		&iam.DeleteAccessKeyInput{
			UserName:    aws.String(testAWSUser),
			AccessKeyId: aws.String(accessKeyID),
		}).Return(&iam.DeleteAccessKeyOutput{}, nil)
}
func mockPutUserPolicy(mockAWSClient *mockaws.MockClient) {
	mockAWSClient.EXPECT().PutUserPolicy(gomock.Any()).Return(&iam.PutUserPolicyOutput{}, nil)
}

func base64DecodeOrFail(t *testing.T, data []byte) string {
	decoded, err := base64.StdEncoding.DecodeString(string(data))
	if err != nil {
		t.Logf("error decoding base64")
		t.Fail()
		return ""
	} else {
		return string(decoded)
	}

}
