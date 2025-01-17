/*
Copyright 2019 The Crossplane Authors.

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

package compute

import (
	"context"
	"encoding/base64"
	"fmt"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/cloudformation"
	"github.com/ghodss/yaml"
	"github.com/google/go-cmp/cmp"
	. "github.com/onsi/gomega"
	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	. "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/crossplaneio/stack-aws/apis"
	. "github.com/crossplaneio/stack-aws/apis/compute/v1alpha2"
	"github.com/crossplaneio/stack-aws/pkg/clients/eks"
	"github.com/crossplaneio/stack-aws/pkg/clients/eks/fake"

	runtimev1alpha1 "github.com/crossplaneio/crossplane-runtime/apis/core/v1alpha1"
	"github.com/crossplaneio/crossplane-runtime/pkg/resource"
	"github.com/crossplaneio/crossplane-runtime/pkg/test"
)

const (
	namespace    = "default"
	providerName = "test-provider"
	clusterName  = "test-cluster"
)

var (
	key = types.NamespacedName{
		Namespace: namespace,
		Name:      clusterName,
	}
	request = reconcile.Request{
		NamespacedName: key,
	}
)

func init() {
	_ = apis.AddToScheme(scheme.Scheme)
}

func testCluster() *EKSCluster {
	return &EKSCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      clusterName,
			Namespace: namespace,
		},
		Spec: EKSClusterSpec{
			ResourceSpec: runtimev1alpha1.ResourceSpec{
				ProviderReference: &corev1.ObjectReference{
					Name: providerName,
				},
			},
		},
	}
}

// assertResource a helper function to check on cluster and its status
func assertResource(g *GomegaWithT, r *Reconciler, s runtimev1alpha1.ConditionedStatus) *EKSCluster {
	rc := &EKSCluster{}
	err := r.Get(ctx, key, rc)
	g.Expect(err).To(BeNil())
	g.Expect(cmp.Diff(s, rc.Status.ConditionedStatus, test.EquateConditions())).Should(BeZero())
	return rc
}

func TestGenerateEksAuth(t *testing.T) {
	g := NewGomegaWithT(t)
	arnName := "test-arn"
	var expectRoles []MapRole
	var expectUsers []MapUser

	defaultMapRole := MapRole{
		RoleARN:  arnName,
		Username: "system:node:{{EC2PrivateDNSName}}",
		Groups:   []string{"system:bootstrappers", "system:nodes"},
	}

	exampleMapRole := MapRole{
		RoleARN:  "arn:aws:iam::000000000000:role/KubernetesAdmin",
		Username: "kubernetes-admin",
		Groups:   []string{"system:masters"},
	}

	exampleMapUser := MapUser{
		UserARN:  "arn:aws:iam::000000000000:user/Alice",
		Username: "alice",
		Groups:   []string{"system:masters"},
	}

	expectRoles = append(expectRoles, exampleMapRole)
	expectUsers = append(expectUsers, exampleMapUser)

	cluster := testCluster()
	cluster.Spec.MapRoles = expectRoles
	cluster.Spec.MapUsers = expectUsers

	// Default is included by so we don't add it to spec
	expectRoles = append(expectRoles, defaultMapRole)

	cm, err := generateAWSAuthConfigMap(cluster, arnName)
	g.Expect(err).To(BeNil())

	g.Expect(cm.Name).To(Equal("aws-auth"))
	g.Expect(cm.Namespace).To(Equal("kube-system"))

	var outputRoles []MapRole
	val := cm.Data["mapRoles"]
	err = yaml.Unmarshal([]byte(val), &outputRoles)
	g.Expect(err).To(BeNil())

	var outputUsers []MapUser
	val = cm.Data["mapUsers"]
	err = yaml.Unmarshal([]byte(val), &outputUsers)
	g.Expect(err).To(BeNil())

	g.Expect(outputRoles).To(Equal(expectRoles))
	g.Expect(outputUsers).To(Equal(expectUsers))
}

func TestCreate(t *testing.T) {
	g := NewGomegaWithT(t)

	test := func(cluster *EKSCluster, client eks.Client, expectedResult reconcile.Result, expectedStatus runtimev1alpha1.ConditionedStatus) *EKSCluster {
		r := &Reconciler{
			Client: NewFakeClient(cluster),
		}

		rs, err := r._create(cluster, client)
		g.Expect(rs).To(Equal(expectedResult))
		g.Expect(err).To(BeNil())
		return assertResource(g, r, expectedStatus)
	}

	// new cluster
	cluster := testCluster()
	cluster.ObjectMeta.UID = types.UID("test-uid")

	client := &fake.MockEKSClient{
		MockCreate: func(string, EKSClusterSpec) (*eks.Cluster, error) { return &eks.Cluster{}, nil },
	}

	expectedStatus := runtimev1alpha1.ConditionedStatus{}
	expectedStatus.SetConditions(runtimev1alpha1.Creating(), runtimev1alpha1.ReconcileSuccess())

	reconciledCluster := test(cluster, client, reconcile.Result{RequeueAfter: aShortWait}, expectedStatus)

	g.Expect(reconciledCluster.Status.ClusterName).To(Equal(fmt.Sprintf("%s%s", clusterNamePrefix, cluster.UID)))
	g.Expect(reconciledCluster.Status.State).To(Equal(ClusterStatusCreating))

	// cluster create error - bad request
	cluster = testCluster()
	cluster.ObjectMeta.UID = types.UID("test-uid")
	errorBadRequest := errors.New("InvalidParameterException")
	client.MockCreate = func(string, EKSClusterSpec) (*eks.Cluster, error) {
		return &eks.Cluster{}, errorBadRequest
	}
	expectedStatus = runtimev1alpha1.ConditionedStatus{}
	expectedStatus.SetConditions(runtimev1alpha1.Creating(), runtimev1alpha1.ReconcileError(errorBadRequest))

	reconciledCluster = test(cluster, client, reconcile.Result{}, expectedStatus)
	g.Expect(reconciledCluster.Finalizers).To(BeEmpty())
	g.Expect(reconciledCluster.Status.ClusterName).To(BeEmpty())
	g.Expect(reconciledCluster.Status.State).To(BeEmpty())
	g.Expect(reconciledCluster.Status.CloudFormationStackID).To(BeEmpty())

	// cluster create error - other
	cluster = testCluster()
	cluster.ObjectMeta.UID = types.UID("test-uid")
	errorOther := errors.New("other")
	client.MockCreate = func(string, EKSClusterSpec) (*eks.Cluster, error) {
		return &eks.Cluster{}, errorOther
	}
	expectedStatus = runtimev1alpha1.ConditionedStatus{}
	expectedStatus.SetConditions(runtimev1alpha1.Creating(), runtimev1alpha1.ReconcileError(errorOther))

	reconciledCluster = test(cluster, client, reconcile.Result{RequeueAfter: aShortWait}, expectedStatus)
	g.Expect(reconciledCluster.Finalizers).To(BeEmpty())
	g.Expect(reconciledCluster.Status.ClusterName).To(BeEmpty())
	g.Expect(reconciledCluster.Status.State).To(BeEmpty())
	g.Expect(reconciledCluster.Status.CloudFormationStackID).To(BeEmpty())
}

func TestSync(t *testing.T) {
	g := NewGomegaWithT(t)
	fakeStackID := "fake-stack-id"

	test := func(tc *EKSCluster, cl *fake.MockEKSClient, sec func(*eks.Cluster, *EKSCluster, eks.Client) error, auth func(*eks.Cluster, *EKSCluster, eks.Client, string) error,
		rslt reconcile.Result, exp runtimev1alpha1.ConditionedStatus) *EKSCluster {
		r := &Reconciler{
			Client:  NewFakeClient(tc),
			secret:  sec,
			awsauth: auth,
		}

		rs, err := r._sync(tc, cl)
		g.Expect(rs).To(Equal(rslt))
		g.Expect(err).NotTo(HaveOccurred())
		return assertResource(g, r, exp)
	}

	fakeWorkerARN := "fake-worker-arn"
	mockClusterWorker := eks.ClusterWorkers{
		WorkerStackID: fakeStackID,
		WorkerARN:     fakeWorkerARN,
	}

	// error retrieving the cluster
	errorGet := errors.New("retrieving cluster")
	cl := &fake.MockEKSClient{
		MockGet: func(string) (*eks.Cluster, error) {
			return nil, errorGet
		},
		MockCreateWorkerNodes: func(string, string, EKSClusterSpec) (*eks.ClusterWorkers, error) { return &mockClusterWorker, nil },
	}

	cl.MockGetWorkerNodes = func(string) (*eks.ClusterWorkers, error) {
		return &eks.ClusterWorkers{
			WorkersStatus: cloudformation.StackStatusCreateInProgress,
			WorkerReason:  "",
			WorkerStackID: fakeStackID}, nil
	}

	expectedStatus := runtimev1alpha1.ConditionedStatus{}
	expectedStatus.SetConditions(runtimev1alpha1.ReconcileError(errorGet))
	tc := testCluster()
	test(tc, cl, nil, nil, reconcile.Result{RequeueAfter: aShortWait}, expectedStatus)

	// cluster is not ready
	cl.MockGet = func(string) (*eks.Cluster, error) {
		return &eks.Cluster{
			Status: ClusterStatusCreating,
		}, nil
	}
	expectedStatus = runtimev1alpha1.ConditionedStatus{}
	tc = testCluster()
	test(tc, cl, nil, nil, reconcile.Result{RequeueAfter: aShortWait}, expectedStatus)

	// cluster is ready, but lets create workers that error
	cl.MockGet = func(string) (*eks.Cluster, error) {
		return &eks.Cluster{
			Status: ClusterStatusActive,
		}, nil
	}

	errorCreateNodes := errors.New("create nodes")
	cl.MockCreateWorkerNodes = func(string, string, EKSClusterSpec) (*eks.ClusterWorkers, error) {
		return nil, errorCreateNodes
	}

	expectedStatus = runtimev1alpha1.ConditionedStatus{}
	expectedStatus.SetConditions(runtimev1alpha1.ReconcileError(errorCreateNodes))
	tc = testCluster()
	reconciledCluster := test(tc, cl, nil, nil, reconcile.Result{RequeueAfter: aShortWait}, expectedStatus)
	g.Expect(reconciledCluster.Status.CloudFormationStackID).To(BeEmpty())

	// cluster is ready, lets create workers
	cl.MockGet = func(string) (*eks.Cluster, error) {
		return &eks.Cluster{
			Status: ClusterStatusActive,
		}, nil
	}

	cl.MockCreateWorkerNodes = func(string, string, EKSClusterSpec) (*eks.ClusterWorkers, error) {
		return &eks.ClusterWorkers{WorkerStackID: fakeStackID}, nil
	}

	expectedStatus = runtimev1alpha1.ConditionedStatus{}
	expectedStatus.SetConditions(runtimev1alpha1.ReconcileSuccess())
	tc = testCluster()
	reconciledCluster = test(tc, cl, nil, nil, reconcile.Result{RequeueAfter: aShortWait}, expectedStatus)
	g.Expect(reconciledCluster.Status.CloudFormationStackID).To(Equal(fakeStackID))

	// cluster is ready, but auth sync failed
	cl.MockGetWorkerNodes = func(string) (*eks.ClusterWorkers, error) {
		return &eks.ClusterWorkers{
			WorkersStatus: cloudformation.StackStatusCreateComplete,
			WorkerReason:  "",
			WorkerStackID: fakeStackID,
			WorkerARN:     fakeWorkerARN,
		}, nil
	}

	errorAuth := errors.New("auth")
	expectedStatus = runtimev1alpha1.ConditionedStatus{}
	expectedStatus.SetConditions(runtimev1alpha1.ReconcileError(errors.Wrap(errorAuth, "failed to set auth map on eks")))
	tc = testCluster()
	tc.Status.CloudFormationStackID = fakeStackID
	auth := func(*eks.Cluster, *EKSCluster, eks.Client, string) error {
		return errorAuth

	}
	test(tc, cl, nil, auth, reconcile.Result{RequeueAfter: aShortWait}, expectedStatus)

	// cluster is ready, but secret failed
	cl.MockGetWorkerNodes = func(string) (*eks.ClusterWorkers, error) {
		return &eks.ClusterWorkers{
			WorkersStatus: cloudformation.StackStatusCreateComplete,
			WorkerReason:  "",
			WorkerStackID: fakeStackID,
			WorkerARN:     fakeWorkerARN,
		}, nil
	}

	auth = func(*eks.Cluster, *EKSCluster, eks.Client, string) error {
		return nil
	}

	errorSecret := errors.New("secret")
	fSec := func(*eks.Cluster, *EKSCluster, eks.Client) error {
		return errorSecret
	}
	expectedStatus = runtimev1alpha1.ConditionedStatus{}
	expectedStatus.SetConditions(runtimev1alpha1.ReconcileError(errorSecret))
	tc = testCluster()
	tc.Status.CloudFormationStackID = fakeStackID
	test(tc, cl, fSec, auth, reconcile.Result{RequeueAfter: aShortWait}, expectedStatus)

	// cluster is ready
	fSec = func(*eks.Cluster, *EKSCluster, eks.Client) error {
		return nil
	}
	expectedStatus = runtimev1alpha1.ConditionedStatus{}
	expectedStatus.SetConditions(runtimev1alpha1.Available(), runtimev1alpha1.ReconcileSuccess())
	tc = testCluster()
	tc.Status.CloudFormationStackID = fakeStackID
	test(tc, cl, fSec, auth, reconcile.Result{RequeueAfter: aLongWait}, expectedStatus)
}

func TestSecret(t *testing.T) {
	clusterCA := []byte("test-ca")
	cluster := &eks.Cluster{
		Status:   ClusterStatusActive,
		Endpoint: "test-ep",
		CA:       base64.StdEncoding.EncodeToString(clusterCA),
	}

	r := &Reconciler{
		publisher: resource.ManagedConnectionPublisherFns{
			PublishConnectionFn: func(_ context.Context, _ resource.Managed, got resource.ConnectionDetails) error {
				want := resource.ConnectionDetails{
					runtimev1alpha1.ResourceCredentialsSecretEndpointKey: []byte(cluster.Endpoint),
					runtimev1alpha1.ResourceCredentialsSecretCAKey:       clusterCA,
					runtimev1alpha1.ResourceCredentialsTokenKey:          []byte("test-token"),
				}

				if diff := cmp.Diff(want, got); diff != "" {
					t.Errorf("-want, +got\n%s", diff)
				}

				return nil
			},
		},
	}

	tc := testCluster()
	client := &fake.MockEKSClient{}

	// Ensure we return an error when we can't get a new token.
	testError := "test-connection-token-error"
	client.MockConnectionToken = func(string) (string, error) { return "", errors.New(testError) }
	want := errors.New(testError)
	got := r._secret(cluster, tc, client)
	if diff := cmp.Diff(want, got, test.EquateErrors()); diff != "" {
		t.Errorf("r._secret(...): -want error, +got error:\n%s", diff)
	}

	// Ensure we don't return an error when we can get a new token.
	client.MockConnectionToken = func(string) (string, error) { return "test-token", nil }
	if err := r._secret(cluster, tc, client); err != nil {
		t.Errorf("r._secret(...): %s", err)
	}
}

func TestDelete(t *testing.T) {
	g := NewGomegaWithT(t)

	test := func(cluster *EKSCluster, client eks.Client, expectedResult reconcile.Result, expectedStatus runtimev1alpha1.ConditionedStatus) *EKSCluster {
		r := &Reconciler{
			Client: NewFakeClient(cluster),
		}

		rs, err := r._delete(cluster, client)
		g.Expect(rs).To(Equal(expectedResult))
		g.Expect(err).To(BeNil())
		return assertResource(g, r, expectedStatus)
	}

	// reclaim - delete
	cluster := testCluster()
	cluster.Finalizers = []string{finalizer}
	cluster.Spec.ReclaimPolicy = runtimev1alpha1.ReclaimDelete
	cluster.Status.CloudFormationStackID = "fake-stack-id"
	cluster.Status.SetConditions(runtimev1alpha1.Available())

	client := &fake.MockEKSClient{}
	client.MockDelete = func(string) error { return nil }
	client.MockDeleteWorkerNodes = func(string) error { return nil }

	expectedStatus := runtimev1alpha1.ConditionedStatus{}
	expectedStatus.SetConditions(runtimev1alpha1.Deleting(), runtimev1alpha1.ReconcileSuccess())

	reconciledCluster := test(cluster, client, reconcile.Result{}, expectedStatus)
	g.Expect(reconciledCluster.Finalizers).To(BeEmpty())

	// reclaim - retain
	cluster.Spec.ReclaimPolicy = runtimev1alpha1.ReclaimRetain
	cluster.Status.SetConditions(runtimev1alpha1.Available())
	cluster.Finalizers = []string{finalizer}
	client.MockDelete = nil // should not be called

	reconciledCluster = test(cluster, client, reconcile.Result{}, expectedStatus)
	g.Expect(reconciledCluster.Finalizers).To(BeEmpty())

	// reclaim - delete, delete error
	cluster.Spec.ReclaimPolicy = runtimev1alpha1.ReclaimDelete
	cluster.Status.SetConditions(runtimev1alpha1.Available())
	cluster.Finalizers = []string{finalizer}
	errorDelete := errors.New("test-delete-error")
	client.MockDelete = func(string) error { return errorDelete }
	expectedStatus = runtimev1alpha1.ConditionedStatus{}
	expectedStatus.SetConditions(
		runtimev1alpha1.Deleting(),
		runtimev1alpha1.ReconcileError(errors.Wrap(errorDelete, "Master Delete Error")),
	)

	reconciledCluster = test(cluster, client, reconcile.Result{RequeueAfter: aShortWait}, expectedStatus)
	g.Expect(reconciledCluster.Finalizers).To(ContainElement(finalizer))

	// reclaim - delete, delete error with worker
	cluster.Spec.ReclaimPolicy = runtimev1alpha1.ReclaimDelete
	cluster.Status.SetConditions(runtimev1alpha1.Available())
	cluster.Finalizers = []string{finalizer}
	testErrorWorker := errors.New("test-delete-error-worker")
	client.MockDelete = func(string) error { return nil }
	client.MockDeleteWorkerNodes = func(string) error { return testErrorWorker }
	expectedStatus = runtimev1alpha1.ConditionedStatus{}
	expectedStatus.SetConditions(
		runtimev1alpha1.Deleting(),
		runtimev1alpha1.ReconcileError(errors.Wrap(testErrorWorker, "Worker Delete Error")),
	)

	reconciledCluster = test(cluster, client, reconcile.Result{RequeueAfter: aShortWait}, expectedStatus)
	g.Expect(reconciledCluster.Finalizers).To(ContainElement(finalizer))

	// reclaim - delete, delete error in cluster and cluster workers
	cluster.Spec.ReclaimPolicy = runtimev1alpha1.ReclaimDelete
	cluster.Status.SetConditions(runtimev1alpha1.Available())
	cluster.Finalizers = []string{finalizer}
	client.MockDelete = func(string) error { return nil }
	client.MockDelete = func(string) error { return errorDelete }
	client.MockDeleteWorkerNodes = func(string) error { return testErrorWorker }
	expectedStatus = runtimev1alpha1.ConditionedStatus{}
	expectedStatus.SetConditions(
		runtimev1alpha1.Deleting(),
		runtimev1alpha1.ReconcileError(errors.New("Master Delete Error: test-delete-error, Worker Delete Error: test-delete-error-worker")),
	)

	reconciledCluster = test(cluster, client, reconcile.Result{RequeueAfter: aShortWait}, expectedStatus)
	g.Expect(reconciledCluster.Finalizers).To(ContainElement(finalizer))
}

func TestReconcileObjectNotFound(t *testing.T) {
	g := NewGomegaWithT(t)

	r := &Reconciler{
		Client: NewFakeClient(),
	}
	rs, err := r.Reconcile(request)
	g.Expect(rs).To(Equal(reconcile.Result{}))
	g.Expect(err).To(BeNil())
}

func TestReconcileClientError(t *testing.T) {
	g := NewGomegaWithT(t)

	testError := errors.New("test-client-error")

	called := false

	r := &Reconciler{
		Client: NewFakeClient(testCluster()),
		connect: func(*EKSCluster) (eks.Client, error) {
			called = true
			return nil, testError
		},
	}

	// expected to have a failed condition
	expectedStatus := runtimev1alpha1.ConditionedStatus{}
	expectedStatus.SetConditions(runtimev1alpha1.ReconcileError(testError))

	rs, err := r.Reconcile(request)
	g.Expect(rs).To(Equal(reconcile.Result{RequeueAfter: aShortWait}))
	g.Expect(err).To(BeNil())
	g.Expect(called).To(BeTrue())

	assertResource(g, r, expectedStatus)
}

func TestReconcileDelete(t *testing.T) {
	g := NewGomegaWithT(t)

	// test objects
	tc := testCluster()
	dt := metav1.Now()
	tc.DeletionTimestamp = &dt

	called := false

	r := &Reconciler{
		Client: NewFakeClient(tc),
		connect: func(*EKSCluster) (eks.Client, error) {
			return nil, nil
		},
		delete: func(*EKSCluster, eks.Client) (reconcile.Result, error) {
			called = true
			return reconcile.Result{}, nil
		},
	}

	rs, err := r.Reconcile(request)
	g.Expect(rs).To(Equal(reconcile.Result{}))
	g.Expect(err).To(BeNil())
	g.Expect(called).To(BeTrue())
	assertResource(g, r, runtimev1alpha1.ConditionedStatus{})
}

func TestReconcileCreate(t *testing.T) {
	g := NewGomegaWithT(t)

	called := false

	r := &Reconciler{
		Client: NewFakeClient(testCluster()),
		connect: func(*EKSCluster) (eks.Client, error) {
			return nil, nil
		},
		create: func(*EKSCluster, eks.Client) (reconcile.Result, error) {
			called = true
			return reconcile.Result{RequeueAfter: aShortWait}, nil
		},
	}

	rs, err := r.Reconcile(request)
	g.Expect(rs).To(Equal(reconcile.Result{RequeueAfter: aShortWait}))
	g.Expect(err).To(BeNil())
	g.Expect(called).To(BeTrue())

	assertResource(g, r, runtimev1alpha1.ConditionedStatus{})
}

func TestReconcileSync(t *testing.T) {
	g := NewGomegaWithT(t)

	called := false

	tc := testCluster()
	tc.Status.ClusterName = "test-status- cluster-name"
	tc.Finalizers = []string{finalizer}

	r := &Reconciler{
		Client: NewFakeClient(tc),
		connect: func(*EKSCluster) (eks.Client, error) {
			return nil, nil
		},
		sync: func(*EKSCluster, eks.Client) (reconcile.Result, error) {
			called = true
			return reconcile.Result{RequeueAfter: aShortWait}, nil
		},
	}

	rs, err := r.Reconcile(request)
	g.Expect(rs).To(Equal(reconcile.Result{RequeueAfter: aShortWait}))
	g.Expect(err).To(BeNil())
	g.Expect(called).To(BeTrue())

	rc := assertResource(g, r, runtimev1alpha1.ConditionedStatus{})
	g.Expect(rc.Finalizers).To(HaveLen(1))
	g.Expect(rc.Finalizers).To(ContainElement(finalizer))
}
