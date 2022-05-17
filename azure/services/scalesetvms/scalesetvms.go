/*
Copyright 2021 The Kubernetes Authors.

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

package scalesetvms

import (
	"context"
	"fmt"
	"strings"
	"time"

	azure2 "github.com/Azure/go-autorest/autorest/azure"
	"github.com/go-logr/logr"
	"github.com/pkg/errors"
	infrav1 "sigs.k8s.io/cluster-api-provider-azure/api/v1beta1"
	"sigs.k8s.io/cluster-api-provider-azure/azure"
	"sigs.k8s.io/cluster-api-provider-azure/azure/converters"
	"sigs.k8s.io/cluster-api-provider-azure/azure/services/virtualmachines"
	"sigs.k8s.io/cluster-api-provider-azure/util/tele"
)

const serviceName = "scalesetvms"

type (
	// ScaleSetVMScope defines the scope interface for a scale sets service.
	ScaleSetVMScope interface {
		azure.ClusterDescriber
		azure.AsyncStatusUpdater
		InstanceID() string
		ProviderID() string
		ScaleSetName() string
		SetVMSSVM(vmssvm *azure.VMSSVM)
	}

	// Service provides operations on Azure resources.
	Service struct {
		Client   client
		VMClient virtualmachines.Client
		Scope    ScaleSetVMScope
	}
)

// NewService creates a new service.
func NewService(scope ScaleSetVMScope) *Service {
	return &Service{
		Client:   newClient(scope),
		VMClient: virtualmachines.NewClient(scope),
		Scope:    scope,
	}
}

// Name returns the service name.
func (s *Service) Name() string {
	return serviceName
}

// Reconcile idempotently gets, creates, and updates a scale set.
func (s *Service) Reconcile(ctx context.Context) error {
	ctx, log, done := tele.StartSpanWithLogger(ctx, "scalesetvms.Service.Reconcile")
	defer done()

	var (
		resourceGroup = s.Scope.ResourceGroup()
		vmssName      = s.Scope.ScaleSetName()
		instanceID    = s.Scope.InstanceID()
		providerID    = s.Scope.ProviderID()
	)

	if instanceID == "" {
		log.V(4).Info("VMSS is flex", "vmssName", vmssName, "providerID", providerID)
		// this means we are using VMSS Flex and need to fetch by resource ID
		// fetch the latest data about the vm -- model mutations are handled by the AzureMachinePoolReconciler
		resourceID := strings.TrimPrefix(providerID, azure.ProviderIDPrefix)
		resourceIDSplits := strings.Split(resourceID, "/")
		resourceID = strings.TrimSuffix(resourceID, resourceIDSplits[len(resourceIDSplits)-1])
		resourceID = resourceID + resourceIDSplits[len(resourceIDSplits)-3] + "_" + resourceIDSplits[len(resourceIDSplits)-1]
		vm, err := s.VMClient.GetByID(ctx, resourceID)
		if err != nil {
			if azure.ResourceNotFound(err) {
				return azure.WithTransientError(errors.New("vm does not exist yet"), 30*time.Second)
			}
			return errors.Wrap(err, "failed getting vm")
		}

		s.Scope.SetVMSSVM(converters.SDKVMToVMSSVM(vm))
		return nil
	}

	log.V(4).Info("VMSS is uniform", "instanceID", instanceID, "vmssName", vmssName, "providerID", providerID)
	// must be using VMSS Uniform, so fetch by vm ID
	// fetch the latest data about the vm -- model mutations are handled by the AzureMachinePoolReconciler
	vm, err := s.Client.Get(ctx, resourceGroup, vmssName, instanceID)
	if err != nil {
		if azure.ResourceNotFound(err) {
			return azure.WithTransientError(errors.New("vm does not exist yet"), 30*time.Second)
		}
		return errors.Wrap(err, "failed getting vm")
	}

	s.Scope.SetVMSSVM(converters.SDKToVMSSVM(vm))
	return nil
}

// Delete deletes a scaleset vm asynchronously returning a future which encapsulates the long-running operation.
func (s *Service) Delete(ctx context.Context) error {
	var (
		resourceGroup = s.Scope.ResourceGroup()
		vmssName      = s.Scope.ScaleSetName()
		instanceID    = s.Scope.InstanceID()
		providerID    = s.Scope.ProviderID()
	)

	ctx, log, done := tele.StartSpanWithLogger(
		ctx,
		"scalesetvms.Service.Delete",
		tele.KVP("resourceGroup", resourceGroup),
		tele.KVP("scaleset", vmssName),
		tele.KVP("instanceID", instanceID),
	)
	defer done()

	if instanceID == "" {
		// this is a VMSS Flex VM
		return s.deleteVMSSFlexVM(ctx, strings.TrimPrefix(providerID, azure.ProviderIDPrefix))
	}

	// this is a VMSS Uniform vm
	return s.deleteVMSSUniformInstance(ctx, resourceGroup, vmssName, instanceID, log)
}

func (s *Service) deleteVMSSFlexVM(ctx context.Context, resourceID string) error {
	ctx, log, done := tele.StartSpanWithLogger(ctx, "scalesetvms.Service.deleteVMSSFlexVM")
	defer done()

	defer func() {
		if vm, err := s.VMClient.GetByID(ctx, resourceID); err == nil && vm.VirtualMachineProperties != nil {
			log.V(4).Info("updating vmss vm state", "state", vm.ProvisioningState)
			s.Scope.SetVMSSVM(converters.SDKVMToVMSSVM(vm))
		}
	}()

	parsed, err := azure2.ParseResourceID(resourceID)
	if err != nil {
		return errors.Wrap(err, fmt.Sprintf("failed to parse resource id %q", resourceID))
	}

	resourceGroup := parsed.ResourceGroup
	resourceName := strings.TrimPrefix(s.Scope.ProviderID(), azure.ProviderIDPrefix)
	resourceNameSplits := strings.Split(resourceName, "/")
	resourceName = resourceNameSplits[len(resourceNameSplits)-3] + "_" + resourceNameSplits[len(resourceNameSplits)-1]

	log.V(4).Info("entering delete")
	future := s.Scope.GetLongRunningOperationState(resourceName, serviceName)
	if future != nil {
		if future.Type != infrav1.DeleteFuture {
			return azure.WithTransientError(errors.New("attempting to delete, non-delete operation in progress"), 30*time.Second)
		}

		log.V(4).Info("checking if the vm is done deleting")
		if _, err := s.VMClient.GetResultIfDone(ctx, future); err != nil {
			// fetch vm to update status
			return errors.Wrap(err, "failed to get result of long running operation")
		}

		// there was no error in fetching the result, the future has been completed
		log.V(4).Info("successfully deleted the vm")
		s.Scope.DeleteLongRunningOperationState(resourceName, serviceName)
		return nil
	}
	// since the future was nil, there is no ongoing activity; start deleting the vm
	log.V(4).Info("vmss delete vm future is nil") // This is always true

	vmGetter := &VMSSFlexVMGetter{
		Name:          resourceName,
		ResourceGroup: resourceGroup,
	}

	sdkFuture, err := s.VMClient.DeleteAsync(ctx, vmGetter)
	if err != nil {
		if azure.ResourceNotFound(err) {
			// already deleted
			return nil
		}
		return errors.Wrapf(err, "failed to delete vm %s/%s", resourceGroup, resourceName)
	}

	if sdkFuture != nil {
		future, err = converters.SDKToFuture(sdkFuture, infrav1.DeleteFuture, serviceName, vmGetter.ResourceName(), vmGetter.ResourceGroupName())
		if err != nil {
			return errors.Wrapf(err, "failed to covert SDK to Future %s/%s", resourceGroup, resourceName)
		}
		s.Scope.SetLongRunningOperationState(future)
		return nil
	}

	s.Scope.DeleteLongRunningOperationState(resourceName, serviceName)
	return nil
}

func (s *Service) deleteVMSSUniformInstance(ctx context.Context, resourceGroup string, vmssName string, instanceID string, log logr.Logger) error {
	ctx, log, done := tele.StartSpanWithLogger(ctx, "scalesetvms.Service.deleteVMSSUniformInstance")
	defer done()

	defer func() {
		if vm, err := s.Client.Get(ctx, resourceGroup, vmssName, instanceID); err == nil && vm.VirtualMachineScaleSetVMProperties != nil {
			log.V(4).Info("updating vmss vm state", "state", vm.ProvisioningState)
			s.Scope.SetVMSSVM(converters.SDKToVMSSVM(vm))
		}
	}()

	log.V(4).Info("entering delete")
	future := s.Scope.GetLongRunningOperationState(instanceID, serviceName)
	if future != nil {
		if future.Type != infrav1.DeleteFuture {
			return azure.WithTransientError(errors.New("attempting to delete, non-delete operation in progress"), 30*time.Second)
		}

		log.V(4).Info("checking if the vm is done deleting")
		if _, err := s.Client.GetResultIfDone(ctx, future); err != nil {
			// fetch vm to update status
			return errors.Wrap(err, "failed to get result of long running operation")
		}

		// there was no error in fetching the result, the future has been completed
		log.V(4).Info("successfully deleted the vm")
		s.Scope.DeleteLongRunningOperationState(instanceID, serviceName)
		return nil
	}

	// since the future was nil, there is no ongoing activity; start deleting the vm
	future, err := s.Client.DeleteAsync(ctx, resourceGroup, vmssName, instanceID)
	if err != nil {
		if azure.ResourceNotFound(err) {
			// already deleted
			return nil
		}
		return errors.Wrapf(err, "failed to delete vm %s/%s", vmssName, instanceID)
	}

	s.Scope.SetLongRunningOperationState(future)

	log.V(4).Info("checking if the vm is done deleting")
	if _, err := s.Client.GetResultIfDone(ctx, future); err != nil {
		// fetch vm to update status
		return errors.Wrap(err, "failed to get result of long running operation")
	}

	s.Scope.DeleteLongRunningOperationState(instanceID, serviceName)
	return nil
}

// VMSSFlexVMGetter is an interface for getting all the required information to create/update/delete an Azure resource.
type VMSSFlexVMGetter struct {
	Name          string
	ResourceGroup string
}

// ResourceName returns the name of the resource.
func (vm *VMSSFlexVMGetter) ResourceName() string {
	return vm.Name
}

// OwnerResourceName returns the name of the resource that owns the resource
// in the case that the resource is an Azure subresource.
func (vm *VMSSFlexVMGetter) OwnerResourceName() string {
	return ""
}

// ResourceGroupName returns the name of the resource group the resource is in.
func (vm *VMSSFlexVMGetter) ResourceGroupName() string {
	return vm.ResourceGroup
}

// Parameters takes the existing resource and returns the desired parameters of the resource.
// If the resource does not exist, or we do not care about existing parameters to update the resource, existing should be nil.
// If no update is needed on the resource, Parameters should return nil.
func (vm *VMSSFlexVMGetter) Parameters(existing interface{}) (params interface{}, err error) {
	return nil, nil
}
