/*
Copyright 2020 Red Hat, Inc.

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
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"time"

	v1alpha1 "github.com/red-hat-storage/ocs-consumer-operator/api/v1alpha1"

	providerclient "github.com/red-hat-storage/ocs-operator/services/provider/client"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

// StorageClassClaimReconciler reconciles a StorageClassClaim object
// nolint
type StorageClassClaimReconciler struct {
	client.Client
	Scheme            *runtime.Scheme
	OperatorNamespace string

	log                   logr.Logger
	ctx                   context.Context
	storageConsumerClient *v1alpha1.StorageConsumerClient
	storageClassClaim     *v1alpha1.StorageClassClaim
}

const (
	StorageClassClaimFinalizer  = "storageclassclaim.odf.openshift.io"
	StorageClassClaimAnnotation = "odf.openshift.io/storagesclassclaim"
)

// +kubebuilder:rbac:groups=odf.openshift.io,resources=storageclassclaims,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=odf.openshift.io,resources=storageclassclaims/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=storage.k8s.io,resources=storageclasses,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=persistentvolumes,verbs=get;list;watch

func (r *StorageClassClaimReconciler) SetupWithManager(mgr ctrl.Manager) error {
	enqueueStorageConsumerRequest := handler.EnqueueRequestsFromMapFunc(
		func(obj client.Object) []reconcile.Request {
			annotations := obj.GetAnnotations()
			if annotation, found := annotations[StorageClassClaimAnnotation]; found {
				parts := strings.Split(annotation, "/")
				return []reconcile.Request{{
					NamespacedName: types.NamespacedName{
						Namespace: parts[0],
						Name:      parts[1],
					},
				}}
			}
			return []reconcile.Request{}
		})
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.StorageClassClaim{}, builder.WithPredicates(
			predicate.GenerationChangedPredicate{},
		)).
		Watches(&source.Kind{Type: &storagev1.StorageClass{}}, enqueueStorageConsumerRequest).
		Complete(r)
}

func (r *StorageClassClaimReconciler) Reconcile(ctx context.Context, request reconcile.Request) (reconcile.Result, error) {
	r.log = ctrllog.FromContext(ctx, "StorageClassClaim", request)
	r.ctx = ctrllog.IntoContext(ctx, r.log)
	r.log.Info("Reconciling StorageClassClaim.")

	// Fetch the StorageClassClaim instance
	r.storageClassClaim = &v1alpha1.StorageClassClaim{}
	r.storageClassClaim.Name = request.Name
	r.storageClassClaim.Namespace = request.Namespace

	if err := r.get(r.storageClassClaim); err != nil {
		if errors.IsNotFound(err) {
			r.log.Info("StorageClassClaim resource not found. Ignoring since object must be deleted.")
			return reconcile.Result{}, nil
		}
		r.log.Error(err, "Failed to get StorageClassClaim.")
		return reconcile.Result{}, err
	}

	r.storageClassClaim.Status.Phase = v1alpha1.StorageClassClaimInitializing

	storageConsumerClientList := &v1alpha1.StorageConsumerClientList{}
	if err := r.list(storageConsumerClientList, client.InNamespace(r.OperatorNamespace)); err != nil {
		return reconcile.Result{}, err
	}

	switch l := len(storageConsumerClientList.Items); {
	case l == 0:
		return reconcile.Result{}, fmt.Errorf("no StorageConsumerClient found")
	case l != 1:
		return reconcile.Result{}, fmt.Errorf("multiple StorageConsumerClient found")
	}
	r.storageConsumerClient = &storageConsumerClientList.Items[0]

	var result reconcile.Result
	var reconcileError error

	// StorageCluster checks for required fields.
	switch storageConsumerClient := r.storageConsumerClient; {
	case storageConsumerClient.Status.ConsumerID == "":
		return reconcile.Result{}, fmt.Errorf("no external storage consumer id found on the " +
			"StorageConsumerClient status, cannot determine mode")
	case storageConsumerClient.Spec.StorageProviderEndpoint == "":
		return reconcile.Result{}, fmt.Errorf("no external storage provider endpoint found on the " +
			"StorageConsumerClient spec, cannot determine mode")
	}

	result, reconcileError = r.reconcileConsumerPhases()

	// Apply status changes to the StorageClassClaim
	statusError := r.Client.Status().Update(r.ctx, r.storageClassClaim)
	if statusError != nil {
		r.log.Info("Failed to update StorageClassClaim status.")
	}

	// Reconcile errors have higher priority than status update errors
	if reconcileError != nil {
		return result, reconcileError
	}

	if statusError != nil {
		return result, statusError
	}

	return result, nil
}

func (r *StorageClassClaimReconciler) reconcileConsumerPhases() (reconcile.Result, error) {
	r.log.Info("Running StorageClassClaim controller in Consumer Mode")

	providerClient, err := providerclient.NewProviderClient(
		r.ctx,
		r.storageConsumerClient.Spec.StorageProviderEndpoint,
		10*time.Second,
	)
	if err != nil {
		return reconcile.Result{}, err
	}

	// Close client-side connections.
	defer providerClient.Close()

	if r.storageClassClaim.GetDeletionTimestamp().IsZero() {

		// TODO: Phases do not have checks at the moment, in order to make them more predictable and less error-prone, at the expense of increased computation cost.
		// Validation phase.
		r.storageClassClaim.Status.Phase = v1alpha1.StorageClassClaimValidating

		// If a StorageClass already exists:
		// 	StorageClassClaim passes validation and is promoted to the configuring phase if:
		//  * the StorageClassClaim has the same type as the StorageClass.
		// 	* the StorageClassClaim has no encryption method specified when the type is filesystem.
		// 	* the StorageClassClaim has a blockpool type and:
		// 		 * the StorageClassClaim has an encryption method specified.
		// 	  * the StorageClassClaim has the same encryption method as the StorageClass.
		// 	StorageClassClaim fails validation and falls back to a failed phase indefinitely (no reconciliation happens).
		existing := &storagev1.StorageClass{
			ObjectMeta: metav1.ObjectMeta{
				Name: r.storageClassClaim.Name,
			},
		}
		if err = r.get(existing); err == nil {
			sccType := r.storageClassClaim.Spec.Type
			sccEncryptionMethod := r.storageClassClaim.Spec.EncryptionMethod
			_, scIsFSType := existing.Parameters["fsName"]
			scEncryptionMethod, scHasEncryptionMethod := existing.Parameters["encryptionMethod"]
			if !((sccType == "sharedfilesystem" && scIsFSType && !scHasEncryptionMethod) ||
				(sccType == "blockpool" && !scIsFSType && sccEncryptionMethod == scEncryptionMethod)) {
				r.log.Error(fmt.Errorf("storageClassClaim is not compatible with existing StorageClass"),
					"StorageClassClaim validation failed.")
				r.storageClassClaim.Status.Phase = v1alpha1.StorageClassClaimFailed
				return reconcile.Result{}, nil
			}
		} else if err != nil && !errors.IsNotFound(err) {
			return reconcile.Result{}, fmt.Errorf("failed to get StorageClass [%v]: %s", existing.ObjectMeta, err)
		}

		// Configuration phase.
		r.storageClassClaim.Status.Phase = v1alpha1.StorageClassClaimConfiguring

		// Check if finalizers are present, if not, add them.
		if !contains(r.storageClassClaim.GetFinalizers(), StorageClassClaimFinalizer) {
			storageClassClaimRef := klog.KRef(r.storageClassClaim.Name, r.storageClassClaim.Namespace)
			r.log.Info("Finalizer not found for StorageClassClaim. Adding finalizer.", "StorageClassClaim", storageClassClaimRef)
			r.storageClassClaim.SetFinalizers(append(r.storageClassClaim.GetFinalizers(), StorageClassClaimFinalizer))
			if err := r.update(r.storageClassClaim); err != nil {
				return reconcile.Result{}, fmt.Errorf("failed to update StorageClassClaim [%v] with finalizer: %s", storageClassClaimRef, err)
			}
		}

		// storageClassClaimStorageType is the storage type of the StorageClassClaim
		var storageClassClaimStorageType providerclient.StorageType
		switch r.storageClassClaim.Spec.Type {
		case "blockpool":
			storageClassClaimStorageType = providerclient.StorageTypeBlockpool
		case "sharedfilesystem":
			storageClassClaimStorageType = providerclient.StorageTypeSharedfilesystem
		default:
			return reconcile.Result{}, fmt.Errorf("unsupported storage type: %s", r.storageClassClaim.Spec.Type)
		}

		// Call the `FulfillStorageClassClaim` service on the provider server with StorageClassClaim as a request message.
		_, err = providerClient.FulfillStorageClassClaim(
			r.ctx,
			r.storageConsumerClient.Status.ConsumerID,
			r.storageClassClaim.Name,
			r.storageClassClaim.Spec.EncryptionMethod,
			storageClassClaimStorageType,
		)
		if err != nil {
			return reconcile.Result{}, fmt.Errorf("failed to initiate fulfillment of StorageClassClaim: %v", err)
		}

		// Call the `GetStorageClassClaimConfig` service on the provider server with StorageClassClaim as a request message.
		response, err := providerClient.GetStorageClassClaimConfig(
			r.ctx,
			r.storageConsumerClient.Status.ConsumerID,
			r.storageClassClaim.Name,
		)
		if err != nil {
			return reconcile.Result{}, fmt.Errorf("failed to get StorageClassClaim config: %v", err)
		}
		resources := response.ExternalResource
		if resources == nil {
			return reconcile.Result{}, fmt.Errorf("no configuration data received")
		}

		// Go over the received objects and operate on them accordingly.
		for _, resource := range resources {
			data := map[string]string{}
			err = json.Unmarshal(resource.Data, &data)
			if err != nil {
				return reconcile.Result{}, fmt.Errorf("failed to unmarshal StorageClassClaim configuration response: %v", err)
			}

			// Create the received resources, if necessary.
			switch resource.Kind {
			case "Secret":
				secret := &corev1.Secret{}
				secret.Name = resource.Name
				secret.Namespace = r.storageClassClaim.Namespace
				_, err = controllerutil.CreateOrUpdate(r.ctx, r.Client, secret, func() error {
					err := r.own(secret)
					if err != nil {
						return fmt.Errorf("failed to own Secret: %v", err)
					}
					if secret.Data == nil {
						secret.Data = map[string][]byte{}
					}
					for k, v := range data {
						secret.Data[k] = []byte(v)
					}
					return nil
				})
				if err != nil {
					return reconcile.Result{}, fmt.Errorf("failed to create or update secret %v: %s", secret, err)
				}
			case "StorageClass":
				var storageClass *storagev1.StorageClass
				data["csi.storage.k8s.io/provisioner-secret-namespace"] = r.storageClassClaim.Namespace
				data["csi.storage.k8s.io/node-stage-secret-namespace"] = r.storageClassClaim.Namespace
				data["csi.storage.k8s.io/controller-expand-secret-namespace"] = r.storageClassClaim.Namespace

				if resource.Name == "cephfs" {
					storageClass = r.getCephFSStorageClass(data)
				} else if resource.Name == "ceph-rbd" {
					storageClass = r.getCephRBDStorageClass(data)
				}
				storageClassClaimNamespacedName := r.getNamespacedName()

				if annotations := storageClass.GetAnnotations(); annotations == nil {
					storageClass.SetAnnotations(map[string]string{
						StorageClassClaimAnnotation: storageClassClaimNamespacedName,
					})
				} else {
					annotations[StorageClassClaimAnnotation] = storageClassClaimNamespacedName
				}
				err = r.createOrReplaceStorageClass(storageClass)
				if err != nil {
					return reconcile.Result{}, fmt.Errorf("failed to create or update StorageClass: %s", err)
				}
			}
		}

		// Readiness phase.
		// Update the StorageClassClaim status.
		r.storageClassClaim.Status.Phase = v1alpha1.StorageClassClaimReady

		// Initiate deletion phase if the StorageClassClaim exists.
	} else if r.storageClassClaim.UID != "" {

		// Deletion phase.
		// Update the StorageClassClaim status.
		r.storageClassClaim.Status.Phase = v1alpha1.StorageClassClaimDeleting

		// Delete StorageClass.
		// Make sure there are no StorageClass consumers left.
		// Check if StorageClass is in use, if yes, then fail.
		// Wait until all PVs using the StorageClass under deletion are removed.
		// Check for any PVs using the StorageClass.
		pvList := corev1.PersistentVolumeList{}
		err := r.list(&pvList)
		if err != nil {
			return reconcile.Result{}, fmt.Errorf("failed to list PersistentVolumes: %s", err)
		}
		for i := range pvList.Items {
			pv := &pvList.Items[i]
			if pv.Spec.StorageClassName == r.storageClassClaim.Name {
				return reconcile.Result{}, fmt.Errorf("StorageClass %s is still in use by one or more PV(s)",
					r.storageClassClaim.Name)
			}
		}

		// Call `RevokeStorageClassClaim` service on the provider server with StorageClassClaim as a request message.
		// Check if StorageClassClaim is still exists (it might have been manually removed during the StorageClass
		// removal above).
		_, err = providerClient.RevokeStorageClassClaim(
			r.ctx,
			r.storageConsumerClient.Status.ConsumerID,
			r.storageClassClaim.Name,
		)
		if err != nil {
			return reconcile.Result{}, err
		}

		storageClass := &storagev1.StorageClass{}
		storageClass.Name = r.storageClassClaim.Name
		if err = r.get(storageClass); err != nil && !errors.IsNotFound(err) {
			return reconcile.Result{}, fmt.Errorf("failed to get StorageClass %s: %s", storageClass.Name, err)
		}
		if storageClass.UID != "" {

			if err = r.delete(storageClass); err != nil {
				return reconcile.Result{}, fmt.Errorf("failed to delete StorageClass %s: %s", storageClass.Name, err)
			}
		} else {
			r.log.Info("StorageClass already deleted.")
		}
		if contains(r.storageClassClaim.GetFinalizers(), StorageClassClaimFinalizer) {
			r.storageClassClaim.Finalizers = remove(r.storageClassClaim.Finalizers, StorageClassClaimFinalizer)
			if err := r.update(r.storageClassClaim); err != nil {
				return ctrl.Result{}, fmt.Errorf("failed to remove finalizer from storageClassClaim: %s", err)
			}
		}
	}

	return reconcile.Result{}, nil
}

func (r *StorageClassClaimReconciler) getCephFSStorageClass(data map[string]string) *storagev1.StorageClass {
	pvReclaimPolicy := corev1.PersistentVolumeReclaimDelete
	allowVolumeExpansion := true
	storageClass := &storagev1.StorageClass{
		ObjectMeta: metav1.ObjectMeta{
			Name:      r.storageClassClaim.Name,
			Namespace: r.storageClassClaim.Namespace,
			Annotations: map[string]string{
				"description": "Provides RWO and RWX Filesystem volumes",
			},
		},
		ReclaimPolicy:        &pvReclaimPolicy,
		AllowVolumeExpansion: &allowVolumeExpansion,
		Provisioner:          fmt.Sprintf("%s.cephfs.csi.ceph.com", r.storageConsumerClient.Namespace),
		Parameters:           data,
	}
	return storageClass
}

func (r *StorageClassClaimReconciler) getCephRBDStorageClass(data map[string]string) *storagev1.StorageClass {
	pvReclaimPolicy := corev1.PersistentVolumeReclaimDelete
	allowVolumeExpansion := true
	storageClass := &storagev1.StorageClass{
		ObjectMeta: metav1.ObjectMeta{
			Name:      r.storageClassClaim.Name,
			Namespace: r.storageClassClaim.Namespace,
			Annotations: map[string]string{
				"description": "Provides RWO Filesystem volumes, and RWO and RWX Block volumes",
			},
		},
		ReclaimPolicy:        &pvReclaimPolicy,
		AllowVolumeExpansion: &allowVolumeExpansion,
		Provisioner:          fmt.Sprintf("%s.rbd.csi.ceph.com", r.storageConsumerClient.Namespace),
		Parameters:           data,
	}
	return storageClass
}

func (r *StorageClassClaimReconciler) createOrReplaceStorageClass(storageClass *storagev1.StorageClass) error {
	existing := &storagev1.StorageClass{}
	existing.Name = r.storageClassClaim.Name

	if err := r.get(existing); err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("failed to get StorageClass: %v", err)
	}

	// If present then compare the existing StorageClass with the received StorageClass, and only proceed if they differ.
	if reflect.DeepEqual(existing.Parameters, storageClass.Parameters) {
		return nil
	}

	// StorageClass already exists, but parameters have changed. Delete the existing StorageClass and create a new one.
	if existing.UID != "" {

		// Since we have to update the existing StorageClass, so we will delete the existing StorageClass and create a new one.
		r.log.Info("StorageClass needs to be updated, deleting it.", "StorageClass", klog.KRef(storageClass.Namespace, existing.Name))

		// Delete the StorageClass.
		err := r.delete(existing)
		if err != nil {
			r.log.Error(err, "Failed to delete StorageClass.", "StorageClass", klog.KRef(storageClass.Namespace, existing.Name))
			return err
		}
	}
	r.log.Info("Creating StorageClass.", "StorageClass", klog.KRef(storageClass.Namespace, existing.Name))
	err := r.Client.Create(r.ctx, storageClass)
	if err != nil {
		return fmt.Errorf("failed to create StorageClass: %v", err)
	}
	return nil
}

func (r *StorageClassClaimReconciler) get(obj client.Object) error {
	key := client.ObjectKeyFromObject(obj)
	return r.Client.Get(r.ctx, key, obj)
}

func (r *StorageClassClaimReconciler) update(obj client.Object) error {
	return r.Client.Update(r.ctx, obj)
}

func (r *StorageClassClaimReconciler) list(obj client.ObjectList, listOptions ...client.ListOption) error {
	return r.Client.List(r.ctx, obj, listOptions...)
}

func (r *StorageClassClaimReconciler) delete(obj client.Object) error {
	if err := r.Client.Delete(r.ctx, obj); err != nil && !errors.IsNotFound(err) {
		return err
	}
	return nil
}

func (r *StorageClassClaimReconciler) own(resource metav1.Object) error {
	// Ensure StorageClassClaim ownership on a resource
	return controllerutil.SetOwnerReference(r.storageClassClaim, resource, r.Scheme)
}

func (r *StorageClassClaimReconciler) getNamespacedName() string {
	return fmt.Sprintf("%s/%s", r.storageClassClaim.Namespace, r.storageClassClaim.Name)
}
