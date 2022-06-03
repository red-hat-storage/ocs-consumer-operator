/*
Copyright 2022 Red Hat, Inc.

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

package controllers

import (
	"context"
	"fmt"
	"time"

	v1alpha1 "github.com/red-hat-storage/ocs-consumer-operator/api/v1alpha1"

	providerClient "github.com/red-hat-storage/ocs-operator/services/provider/client"

	configv1 "github.com/openshift/api/config/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	// grpcCallNames
	OnboardConsumer       = "OnboardConsumer"
	OffboardConsumer      = "OffboardConsumer"
	UpdateCapacity        = "UpdateCapacity"
	GetStorageConfig      = "GetStorageConfig"
	AcknowledgeOnboarding = "AcknowledgeOnboarding"

	storageConsumerClientAnnotation = "odf.openshift.io/storageconsumerclient"
	storageConsumerClientFinalizer  = "storagecomsumerclient.odf.openshift.io"
)

// StorageConsumerClientReconciler reconciles a StorageConsumerClient object
type StorageConsumerClientReconciler struct {
	client.Client
	Log    klog.Logger
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups=odf.openshift.io,resources=storageconsumerclients,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=odf.openshift.io,resources=storageconsumerclients/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=odf.openshift.io,resources=storageconsumerclients/finalizers,verbs=update
//+kubebuilder:rbac:groups=odf.openshift.io,resources=storageclassclaims,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=odf.openshift.io,resources=storageclassclaims/status,verbs=get;update;patch

// SetupWithManager sets up the controller with the Manager.
func (r *StorageConsumerClientReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.StorageConsumerClient{}).
		Complete(r)
}

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.11.0/pkg/reconcile
func (r *StorageConsumerClientReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var err error

	r.Log = log.FromContext(ctx, "StorageConsumerClient", req)
	r.Log.Info("Reconciling StorageConsumerClient")

	// Fetch the StorageConsumerClient instance
	instance := &v1alpha1.StorageConsumerClient{}
	instance.Name = req.Name
	instance.Namespace = req.Namespace

	if err = r.Client.Get(ctx, types.NamespacedName{Name: instance.Name, Namespace: instance.Namespace}, instance); err != nil {
		if errors.IsNotFound(err) {
			r.Log.Info("StorageConsumerClient resource not found. Ignoring since object must be deleted.")
			return reconcile.Result{}, nil
		}
		r.Log.Error(err, "Failed to get StorageConsumerClient.")
		return reconcile.Result{}, err
	}

	result, reconcileErr := r.reconcilePhases(instance)

	// Apply status changes to the StorageConsumerClient
	statusErr := r.Client.Status().Update(ctx, instance)
	if statusErr != nil {
		r.Log.Info("Failed to update StorageConsumerClient status.")
	}

	if reconcileErr != nil {
		err = reconcileErr
	} else if statusErr != nil {
		err = statusErr
	}
	return result, err
}

func (r *StorageConsumerClientReconciler) reconcilePhases(instance *v1alpha1.StorageConsumerClient) (ctrl.Result, error) {
	externalClusterClient, err := r.newExternalClusterClient(instance)
	if err != nil {
		return reconcile.Result{}, err
	}
	defer externalClusterClient.Close()

	// deletion phase
	if !instance.GetDeletionTimestamp().IsZero() {
		return r.deletionPhase(instance, externalClusterClient)
	}

	instance.Status.Phase = v1alpha1.StorageConsumerInitializing

	// ensure finalizer
	if !contains(instance.GetFinalizers(), storageConsumerClientFinalizer) {
		r.Log.Info("Finalizer not found for StorageConsumerClient. Adding finalizer.", "StorageConsumerClient", klog.KRef(instance.Namespace, instance.Name))
		instance.ObjectMeta.Finalizers = append(instance.ObjectMeta.Finalizers, storageConsumerClientFinalizer)
		if err := r.Client.Update(context.TODO(), instance); err != nil {
			r.Log.Info("Failed to update StorageConsumerClient with finalizer.", "StorageConsumerClient", klog.KRef(instance.Namespace, instance.Name))
			return reconcile.Result{}, err
		}
	}

	if instance.Status.ConsumerID == "" {
		return r.onboardConsumer(instance, externalClusterClient)
	} else if instance.Status.Phase == v1alpha1.StorageConsumerOnboarding {
		return r.acknowledgeOnboarding(instance, externalClusterClient)
	} else if !instance.Spec.RequestedCapacity.Equal(instance.Status.GrantedCapacity) {
		res, err := r.updateConsumerCapacity(instance, externalClusterClient)
		if err != nil || !res.IsZero() {
			return res, err
		}
	}

	return reconcile.Result{}, nil
}

func (r *StorageConsumerClientReconciler) deletionPhase(instance *v1alpha1.StorageConsumerClient, externalClusterClient *providerClient.OCSProviderClient) (ctrl.Result, error) {
	if contains(instance.GetFinalizers(), storageConsumerClientFinalizer) {
		instance.Status.Phase = v1alpha1.StorageConsumerOffboarding

		if res, err := r.offboardConsumer(instance, externalClusterClient); err != nil {
			r.Log.Info("Offboarding in progress.", "Status", err)
			//r.recorder.ReportIfNotPresent(instance, corev1.EventTypeWarning, statusutil.EventReasonUninstallPending, err.Error())
			return reconcile.Result{RequeueAfter: time.Second * time.Duration(1)}, nil
		} else if !res.IsZero() {
			// result is not empty
			return res, nil
		}
		r.Log.Info("removing finalizer from StorageConsumerClient.", "StorageConsumerClient", klog.KRef(instance.Namespace, instance.Name))
		// Once all finalizers have been removed, the object will be deleted
		instance.ObjectMeta.Finalizers = remove(instance.ObjectMeta.Finalizers, storageConsumerClientFinalizer)
		if err := r.Client.Update(context.TODO(), instance); err != nil {
			r.Log.Info("Failed to remove finalizer from StorageConsumerClient", "StorageConsumerClient", klog.KRef(instance.Namespace, instance.Name))
			return reconcile.Result{}, err
		}
	}
	r.Log.Info("StorageConsumerClient is offboarded", "StorageConsumerClient", klog.KRef(instance.Namespace, instance.Name))
	//returnErr := r.SetOperatorConditions("Skipping StorageConsumerClient reconciliation", "Terminated", metav1.ConditionTrue, nil)
	return reconcile.Result{}, nil
}

// newExternalClusterClient returns the *providerClient.OCSProviderClient
func (r *StorageConsumerClientReconciler) newExternalClusterClient(instance *v1alpha1.StorageConsumerClient) (*providerClient.OCSProviderClient, error) {

	consumerClient, err := providerClient.NewProviderClient(
		context.Background(), instance.Spec.StorageProviderEndpoint, time.Second*10)
	if err != nil {
		return nil, err
	}

	return consumerClient, nil
}

// onboardConsumer makes an API call to the external storage provider cluster for onboarding
func (r *StorageConsumerClientReconciler) onboardConsumer(instance *v1alpha1.StorageConsumerClient, externalClusterClient *providerClient.OCSProviderClient) (reconcile.Result, error) {

	clusterVersion := &configv1.ClusterVersion{}
	err := r.Client.Get(context.Background(), types.NamespacedName{Name: "version"}, clusterVersion)
	if err != nil {
		r.Log.Error(err, "failed to get the clusterVersion version of the OCP cluster")
		return reconcile.Result{}, err
	}

	name := fmt.Sprintf("storageconsumer-%s", clusterVersion.Spec.ClusterID)
	response, err := externalClusterClient.OnboardConsumer(
		context.Background(), instance.Spec.OnboardingTicket, name,
		instance.Spec.RequestedCapacity.String())
	if err != nil {
		if s, ok := status.FromError(err); ok {
			r.logGrpcErrorAndReportEvent(instance, OnboardConsumer, err, s.Code())
		}
		return reconcile.Result{}, err
	}

	if response.StorageConsumerUUID == "" || response.GrantedCapacity == "" {
		err = fmt.Errorf("storage provider response is empty")
		r.Log.Error(err, "empty response")
		return reconcile.Result{}, err
	}

	instance.Status.ConsumerID = response.StorageConsumerUUID
	instance.Status.GrantedCapacity = resource.MustParse(response.GrantedCapacity)
	instance.Status.Phase = v1alpha1.StorageConsumerOnboarding

	r.Log.Info("onboarding complete")
	return reconcile.Result{Requeue: true}, nil
}

func (r *StorageConsumerClientReconciler) acknowledgeOnboarding(instance *v1alpha1.StorageConsumerClient, externalClusterClient *providerClient.OCSProviderClient) (reconcile.Result, error) {

	_, err := externalClusterClient.AcknowledgeOnboarding(context.Background(), instance.Status.ConsumerID)
	if err != nil {
		if s, ok := status.FromError(err); ok {
			r.logGrpcErrorAndReportEvent(instance, AcknowledgeOnboarding, err, s.Code())
		}
		r.Log.Error(err, "External-OCS:Failed to acknowledge onboarding.")
		return reconcile.Result{}, err
	}

	// claims should be created only once and should not be created/updated again if user deletes/update it.
	err = r.createDefaultStorageClassClaims(instance)
	if err != nil {
		return reconcile.Result{}, err
	}

	// instance.Status.Phase = statusutil.PhaseProgressing

	r.Log.Info("External-OCS:Onboarding is acknowledged successfully.")
	return reconcile.Result{Requeue: true}, nil
}

// offboardConsumer makes an API call to the external storage provider cluster for offboarding
func (r *StorageConsumerClientReconciler) offboardConsumer(instance *v1alpha1.StorageConsumerClient, externalClusterClient *providerClient.OCSProviderClient) (reconcile.Result, error) {

	_, err := externalClusterClient.OffboardConsumer(context.Background(), instance.Status.ConsumerID)
	if err != nil {
		if s, ok := status.FromError(err); ok {
			r.logGrpcErrorAndReportEvent(instance, OffboardConsumer, err, s.Code())
		}
		return reconcile.Result{}, err
	}

	return reconcile.Result{}, nil
}

// updateConsumerCapacity makes an API call to the external storage provider cluster to update the capacity
func (r *StorageConsumerClientReconciler) updateConsumerCapacity(instance *v1alpha1.StorageConsumerClient, externalClusterClient *providerClient.OCSProviderClient) (reconcile.Result, error) {
	instance.Status.Phase = v1alpha1.StorageConsumerUpdating

	response, err := externalClusterClient.UpdateCapacity(
		context.Background(),
		instance.Status.ConsumerID,
		instance.Spec.RequestedCapacity.String())
	if err != nil {
		if s, ok := status.FromError(err); ok {
			r.logGrpcErrorAndReportEvent(instance, UpdateCapacity, err, s.Code())
		}
		return reconcile.Result{}, err
	}

	responseQuantity, err := resource.ParseQuantity(response.GrantedCapacity)
	if err != nil {
		r.Log.Error(err, "Failed to parse GrantedCapacity from UpdateCapacity response.", "GrantedCapacity", response.GrantedCapacity)
		return reconcile.Result{}, err
	}

	if !instance.Spec.RequestedCapacity.Equal(responseQuantity) {
		klog.Warningf("GrantedCapacity is not equal to the RequestedCapacity in the UpdateCapacity response.",
			"GrantedCapacity", response.GrantedCapacity, "RequestedCapacity", instance.Spec.RequestedCapacity)
	}

	instance.Status.GrantedCapacity = responseQuantity
	instance.Status.Phase = v1alpha1.StorageConsumerConnected

	return reconcile.Result{}, nil
}

/*
// getExternalConfigFromProvider makes an API call to the external storage provider cluster for json blob
func (r *StorageConsumerClientReconciler) getExternalConfigFromProvider(
	instance *v1alpha1.StorageConsumerClient, externalClusterClient *providerClient.OCSProviderClient) ([]ExternalResource, reconcile.Result, error) {

	response, err := externalClusterClient.GetStorageConfig(context.Background(), instance.Status.ConsumerID)
	if err != nil {
		if s, ok := status.FromError(err); ok {
			r.logGrpcErrorAndReportEvent(instance, GetStorageConfig, err, s.Code())

			// storage consumer is not ready yet, requeue after some time
			if s.Code() == codes.Unavailable {
				return nil, reconcile.Result{RequeueAfter: time.Second * 5}, nil
			}
		}

		return nil, reconcile.Result{}, err
	}

	var externalResources []ExternalResource

	for _, eResource := range response.ExternalResource {

		data := map[string]string{}
		err = json.Unmarshal(eResource.Data, &data)
		if err != nil {
			r.Log.Error(err, "Failed to Unmarshal response of GetStorageConfig", "Kind", eResource.Kind, "Name", eResource.Name, "Data", eResource.Data)
			return nil, reconcile.Result{}, err
		}

		externalResources = append(externalResources, ExternalResource{
			Kind: eResource.Kind,
			Data: data,
			Name: eResource.Name,
		})
	}

	return externalResources, reconcile.Result{}, nil
}
*/
func (r *StorageConsumerClientReconciler) logGrpcErrorAndReportEvent(instance *v1alpha1.StorageConsumerClient, grpcCallName string, err error, errCode codes.Code) {

	// var msg, eventReason, eventType string
	var msg string

	if grpcCallName == OnboardConsumer {
		if errCode == codes.InvalidArgument {
			msg = "Token is invalid. Verify the token again or contact the provider admin"
			//eventReason = "TokenInvalid"
			//eventType = corev1.EventTypeWarning
		} else if errCode == codes.AlreadyExists {
			msg = "Token is already used. Contact provider admin for a new token"
			//eventReason = "TokenAlreadyUsed"
			//eventType = corev1.EventTypeWarning
		}
	} else if grpcCallName == AcknowledgeOnboarding {
		if errCode == codes.NotFound {
			msg = "StorageConsumer not found. Contact the provider admin"
			//eventReason = "NotFound"
			//eventType = corev1.EventTypeWarning
		}
	} else if grpcCallName == OffboardConsumer {
		if errCode == codes.InvalidArgument {
			msg = "StorageConsumer UID is not valid. Contact the provider admin"
			//eventReason = "UIDInvalid"
			//eventType = corev1.EventTypeWarning
		}
	} else if grpcCallName == UpdateCapacity {
		if errCode == codes.InvalidArgument {
			msg = "StorageConsumer UID or requested capacity is not valid. Contact the provider admin"
			//eventReason = "UIDorCapacityInvalid"
			//eventType = corev1.EventTypeWarning
		} else if errCode == codes.NotFound {
			msg = "StorageConsumer UID not found. Contact the provider admin"
			//eventReason = "UIDNotFound"
			//eventType = corev1.EventTypeWarning
		}
	} else if grpcCallName == GetStorageConfig {
		if errCode == codes.InvalidArgument {
			msg = "StorageConsumer UID is not valid. Contact the provider admin"
			//eventReason = "UIDInvalid"
			//eventType = corev1.EventTypeWarning
		} else if errCode == codes.NotFound {
			msg = "StorageConsumer UID not found. Contact the provider admin"
			//eventReason = "UIDNotFound"
			//eventType = corev1.EventTypeWarning
		} else if errCode == codes.Unavailable {
			msg = "StorageConsumer is not ready yet. Will requeue after 5 second"
			//eventReason = "NotReady"
			//eventType = corev1.EventTypeNormal
		}
	}

	if msg != "" {
		r.Log.Error(err, "StorageProvider:"+grpcCallName+":"+msg)
		// r.recorder.ReportIfNotPresent(instance, eventType, eventReason, msg)
	}
}

func (r *StorageConsumerClientReconciler) createAndOwnStorageClassClaim(
	instance *v1alpha1.StorageConsumerClient, claim *v1alpha1.StorageClassClaim) error {

	err := controllerutil.SetOwnerReference(instance, claim, r.Client.Scheme())
	if err != nil {
		return err
	}

	err = r.Client.Create(context.TODO(), claim)
	if err != nil && !errors.IsAlreadyExists(err) {
		return err
	}
	return nil
}

// claims should be created only once and should not be created/updated again if user deletes/update it.
func (r *StorageConsumerClientReconciler) createDefaultStorageClassClaims(instance *v1alpha1.StorageConsumerClient) error {

	storageClassClaimFile := &v1alpha1.StorageClassClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      generateNameForCephFilesystemSC(instance.Name),
			Namespace: instance.Namespace,
			Labels:    map[string]string{
				//defaultStorageClassClaimLabel: "true",
			},
		},
		Spec: v1alpha1.StorageClassClaimSpec{
			Type: "sharedfilesystem",
		},
	}

	storageClassClaimBlock := &v1alpha1.StorageClassClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      generateNameForCephBlockPoolSC(instance.Name),
			Namespace: instance.Namespace,
			Labels:    map[string]string{
				//defaultStorageClassClaimLabel: "true",
			},
		},
		Spec: v1alpha1.StorageClassClaimSpec{
			Type: "blockpool",
		},
	}

	err := r.createAndOwnStorageClassClaim(instance, storageClassClaimFile)
	if err != nil {
		return err
	}

	err = r.createAndOwnStorageClassClaim(instance, storageClassClaimBlock)
	if err != nil {
		return err
	}

	return nil
}
