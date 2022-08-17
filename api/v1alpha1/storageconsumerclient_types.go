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

package v1alpha1

import (
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type storageConsumerPhase string

const (
	// StorageConsumerInitializing represents Initializing state of StorageConsumer
	StorageConsumerInitializing storageConsumerPhase = "Initializing"
	// StorageConsumerOnboarding represents Onboarding state of StorageConsumer
	StorageConsumerOnboarding storageConsumerPhase = "Onboarding"
	// StorageConsumerConnected represents Onboarding state of StorageConsumer
	StorageConsumerConnected storageConsumerPhase = "Connected"
	// StorageConsumerUpdating represents Onboarding state of StorageConsumer
	StorageConsumerUpdating storageConsumerPhase = "Updating"
	// StorageConsumerOffboarding represents Onboarding state of StorageConsumer
	StorageConsumerOffboarding storageConsumerPhase = "Offboarding"
)

// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// StorageConsumerClientSpec defines the desired state of StorageConsumerClient
type StorageConsumerClientSpec struct {
	// StorageProviderEndpoint holds info to establish connection with the storage providing cluster.
	StorageProviderEndpoint string `json:"storageProviderEndpoint"`

	// OnboardingTicket holds an identity information required for consumer to onboard.
	OnboardingTicket string `json:"onboardingTicket"`

	// RequestedCapacity Will define the desired capacity requested by a consumer cluster.
	RequestedCapacity *resource.Quantity `json:"requestedCapacity,omitempty"`
}

// StorageConsumerClientStatus defines the observed state of StorageConsumerClient
type StorageConsumerClientStatus struct {
	Phase storageConsumerPhase `json:"phase,omitempty"`

	// GrantedCapacity Will report the actual capacity
	// granted to the consumer cluster by the provider cluster.
	GrantedCapacity resource.Quantity `json:"grantedCapacity,omitempty"`

	// ConsumerID will hold the identity of this cluster inside the attached provider cluster
	ConsumerID string `json:"id,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status

// StorageConsumerClient is the Schema for the storageconsumerclients API
type StorageConsumerClient struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   StorageConsumerClientSpec   `json:"spec,omitempty"`
	Status StorageConsumerClientStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// StorageConsumerClientList contains a list of StorageConsumerClient
type StorageConsumerClientList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []StorageConsumerClient `json:"items"`
}

func init() {
	SchemeBuilder.Register(&StorageConsumerClient{}, &StorageConsumerClientList{})
}
