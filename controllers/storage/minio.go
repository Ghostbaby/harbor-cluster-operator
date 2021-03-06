package storage

import (
	"context"
	"fmt"
	"github.com/go-logr/logr"
	goharborv1 "github.com/goharbor/harbor-cluster-operator/api/v1"
	"github.com/goharbor/harbor-cluster-operator/controllers/k8s"
	"github.com/goharbor/harbor-cluster-operator/lcm"
	"github.com/google/go-cmp/cmp"
	minio "github.com/minio/minio-operator/pkg/apis/operator.min.io/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	k8serror "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
)

const (
	inClusterStorage = "inCluster"
	azureStorage     = "azure"
	gcsStorage       = "gcs"
	s3Storage        = "s3"
	swiftStorage     = "swift"
	ossStorage       = "oss"

	DefaultExternalSecretSuffix = "harbor-cluster-storage"
	DefaultCredsSecret          = "minio-creds"
	ExternalStorageSecretSuffix = "Secret"

	DefaultZone   = "zone-harbor"
	DefaultMinIO  = "minio"
	DefaultRegion = "us-east-1"
	DefaultBucket = "harbor"

	LabelOfStorageType = "storageType"
)

type MinIOReconciler struct {
	HarborCluster         *goharborv1.HarborCluster
	KubeClient            k8s.Client
	Ctx                   context.Context
	Log                   logr.Logger
	Scheme                *runtime.Scheme
	Recorder              record.EventRecorder
	CurrentMinIOCR        *minio.MinIOInstance
	DesiredMinIOCR        *minio.MinIOInstance
	CurrentExternalSecret *corev1.Secret
	DesiredExternalSecret *corev1.Secret
	MinioClient           Minio
}

var (
	HarborClusterMinIOGVK = schema.GroupVersionKind{
		Group:   minio.SchemeGroupVersion.Group,
		Version: minio.SchemeGroupVersion.Version,
		Kind:    minio.MinIOCRDResourceKind,
	}
)

// Reconciler implements the reconcile logic of minIO service
func (m *MinIOReconciler) Reconcile() (*lcm.CRStatus, error) {
	var minioCR minio.MinIOInstance
	if m.HarborCluster.Spec.Storage.Kind != inClusterStorage {
		var exSecret corev1.Secret
		err := m.KubeClient.Get(m.getExternalSecretNamespacedName(), &exSecret)
		if k8serror.IsNotFound(err) {
			return m.ProvisionExternalStorage()
		} else if err != nil {
			return minioNotReadyStatus(GetExternalSecretError, err.Error()), err
		}

		m.CurrentExternalSecret = &exSecret
		m.DesiredExternalSecret, err = m.generateExternalSecret()
		if err != nil {
			return minioNotReadyStatus(err.Error(), err.Error()), err
		}

		if m.checkExternalUpdate() {
			return m.ExternalUpdate()
		}

		return nil, nil
	}

	m.DesiredMinIOCR = m.generateMinIOCR()

	err := m.KubeClient.Get(m.getMinIONamespacedName(), &minioCR)
	if k8serror.IsNotFound(err) {
		return m.Provision()
	} else if err != nil {
		return minioNotReadyStatus(GetMinIOError, err.Error()), err
	}

	m.CurrentMinIOCR = &minioCR

	// TODO remove scale event
	isScale, err := m.checkMinIOScale()
	if err != nil {
		return minioNotReadyStatus(ScaleMinIOError, err.Error()), err
	}
	if isScale {
		return m.Scale()
	}

	if m.checkMinIOUpdate() {
		return m.Update()
	}

	isReady, err := m.checkMinIOReady()
	if err != nil {
		return minioNotReadyStatus(GetMinIOError, err.Error()), err
	}

	if isReady {
		err := m.minioInit()
		if err != nil {
			return minioNotReadyStatus(CreateDefaultBucketError, err.Error()), err
		}
		return m.ProvisionInClusterSecretAsS3(&minioCR)
	}

	return minioUnknownStatus(), nil
}

func (m *MinIOReconciler) minioInit() error {
	accessKey, secretKey, err := m.getCredsFromSecret()
	if err != nil {
		return err
	}
	endpoint := m.getServiceName() + "." + m.HarborCluster.Namespace

	m.MinioClient, err = GetMinioClient(endpoint, string(accessKey), string(secretKey), DefaultRegion, false)
	if err != nil {
		return err
	}

	exists, err := m.MinioClient.IsBucketExists(DefaultBucket)
	if err != nil || exists {
		return err
	}

	err = m.MinioClient.CreateBucket(DefaultBucket)
	return err
}

func (m *MinIOReconciler) checkMinIOUpdate() bool {
	if m.DesiredMinIOCR.Spec.Image != m.CurrentMinIOCR.Spec.Image {
		return true
	}

	return false
}

func (m *MinIOReconciler) checkExternalUpdate() bool {
	return !cmp.Equal(m.DesiredExternalSecret.DeepCopy().Data, m.CurrentExternalSecret.DeepCopy().Data)
}

func (m *MinIOReconciler) checkMinIOScale() (bool, error) {
	currentReplicas := m.CurrentMinIOCR.Spec.Zones[0].Servers
	desiredReplicas := m.HarborCluster.Spec.Storage.InCluster.Spec.Replicas
	if currentReplicas == desiredReplicas {
		return false, nil
	} else if currentReplicas == 1 {
		return false, fmt.Errorf("not support upgrading from standalone to distributed mode")
	}

	// MinIO creates erasure-coding sets of 4 to 16 drives per set.
	// The number of drives you provide in total must be a multiple of one of those numbers.
	// TODO validate by webhook
	if desiredReplicas%2 == 0 && desiredReplicas < 16 {
		return true, nil
	}

	return false, fmt.Errorf("for distributed mode, supply 4 to 16 drives (should be even)")
}

func (m *MinIOReconciler) checkMinIOReady() (bool, error) {
	var minioStatefulSet appsv1.StatefulSet
	err := m.KubeClient.Get(m.getMinIONamespacedName(), &minioStatefulSet)

	if minioStatefulSet.Status.ReadyReplicas == m.HarborCluster.Spec.Storage.InCluster.Spec.Replicas {
		return true, err
	}

	return false, err
}

func (m *MinIOReconciler) getMinIONamespacedName() types.NamespacedName {
	return types.NamespacedName{
		Namespace: m.HarborCluster.Namespace,
		Name:      m.getServiceName(),
	}
}

func (m *MinIOReconciler) getMinIOSecretNamespacedName() types.NamespacedName {
	return types.NamespacedName{
		Namespace: m.HarborCluster.Namespace,
		Name:      m.HarborCluster.Name + "-" + DefaultCredsSecret,
	}
}

func (m *MinIOReconciler) getExternalSecretNamespacedName() types.NamespacedName {
	return types.NamespacedName{
		Namespace: m.HarborCluster.Namespace,
		Name:      m.getExternalSecretName(),
	}
}

func (m *MinIOReconciler) getExternalSecretName() string {
	return m.HarborCluster.Name + "-" + DefaultExternalSecretSuffix
}

func minioNotReadyStatus(reason, message string) *lcm.CRStatus {
	return &lcm.CRStatus{
		Condition: goharborv1.HarborClusterCondition{
			Type:               goharborv1.StorageReady,
			Status:             corev1.ConditionFalse,
			LastTransitionTime: metav1.Now(),
			Reason:             reason,
			Message:            message,
		},
		Properties: nil,
	}
}

func minioUnknownStatus() *lcm.CRStatus {
	return &lcm.CRStatus{
		Condition: goharborv1.HarborClusterCondition{
			Type:               goharborv1.StorageReady,
			Status:             corev1.ConditionUnknown,
			LastTransitionTime: metav1.Now(),
			Reason:             "",
			Message:            "",
		},
		Properties: nil,
	}
}

func minioReadyStatus(properties *lcm.Properties) *lcm.CRStatus {
	return &lcm.CRStatus{
		Condition: goharborv1.HarborClusterCondition{
			Type:               goharborv1.StorageReady,
			Status:             corev1.ConditionTrue,
			LastTransitionTime: metav1.Now(),
			Reason:             "",
			Message:            "",
		},
		Properties: *properties,
	}
}

// TODO Deprecated
func (m *MinIOReconciler) ScaleUp(newReplicas uint64) (*lcm.CRStatus, error) {
	panic("implement me")
}

// TODO Deprecated
func (m *MinIOReconciler) ScaleDown(newReplicas uint64) (*lcm.CRStatus, error) {
	panic("implement me")
}
