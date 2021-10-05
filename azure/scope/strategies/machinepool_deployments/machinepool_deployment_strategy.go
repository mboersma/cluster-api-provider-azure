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

package machinepool

import (
	"context"
	"math/rand"
	"sort"
	"strings"
	"time"

	"github.com/pkg/errors"
	"k8s.io/apimachinery/pkg/util/intstr"
	infrav1 "sigs.k8s.io/cluster-api-provider-azure/api/v1beta1"
	infrav1exp "sigs.k8s.io/cluster-api-provider-azure/exp/api/v1beta1"
	"sigs.k8s.io/cluster-api-provider-azure/util/tele"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
)

type (
	// Surger is the ability to surge a number of replica.
	Surger interface {
		Surge(desiredReplicaCount int) (int, error)
	}

	// DeleteSelector is the ability to select nodes to be delete with respect to a desired number of replicas.
	DeleteSelector interface {
		SelectMachinesToDelete(ctx context.Context, desiredReplicas int32, machinesByProviderID map[string]infrav1exp.AzureMachinePoolMachine) ([]infrav1exp.AzureMachinePoolMachine, error)
	}

	// TypedDeleteSelector is the ability to select nodes to be deleted with respect to a desired number of nodes, and
	// the ability to describe the underlying type of the deployment strategy.
	TypedDeleteSelector interface {
		DeleteSelector
		Type() infrav1exp.AzureMachinePoolDeploymentStrategyType
	}

	rollingUpdateStrategy struct {
		infrav1exp.MachineRollingUpdateDeployment
	}
)

// NewMachinePoolDeploymentStrategy constructs a strategy implementation described in the AzureMachinePoolDeploymentStrategy
// specification.
func NewMachinePoolDeploymentStrategy(strategy infrav1exp.AzureMachinePoolDeploymentStrategy) TypedDeleteSelector {
	switch strategy.Type {
	case infrav1exp.RollingUpdateAzureMachinePoolDeploymentStrategyType:
		rollingUpdate := strategy.RollingUpdate
		if rollingUpdate == nil {
			rollingUpdate = &infrav1exp.MachineRollingUpdateDeployment{}
		}

		return &rollingUpdateStrategy{
			MachineRollingUpdateDeployment: *rollingUpdate,
		}
	default:
		// default to a rolling update strategy if unknown type
		return &rollingUpdateStrategy{
			MachineRollingUpdateDeployment: infrav1exp.MachineRollingUpdateDeployment{},
		}
	}
}

// Type is the AzureMachinePoolDeploymentStrategyType for the strategy.
func (rollingUpdateStrategy *rollingUpdateStrategy) Type() infrav1exp.AzureMachinePoolDeploymentStrategyType {
	return infrav1exp.RollingUpdateAzureMachinePoolDeploymentStrategyType
}

// Surge calculates the number of replicas that can be added during an upgrade operation.
func (rollingUpdateStrategy *rollingUpdateStrategy) Surge(desiredReplicaCount int) (int, error) {
	if rollingUpdateStrategy.MaxSurge == nil {
		return 1, nil
	}

	return intstr.GetScaledValueFromIntOrPercent(rollingUpdateStrategy.MaxSurge, desiredReplicaCount, true)
}

// maxUnavailable calculates the maximum number of replicas which can be unavailable at any time.
func (rollingUpdateStrategy *rollingUpdateStrategy) maxUnavailable(desiredReplicaCount int) (int, error) {
	if rollingUpdateStrategy.MaxUnavailable != nil {
		val, err := intstr.GetScaledValueFromIntOrPercent(rollingUpdateStrategy.MaxUnavailable, desiredReplicaCount, false)
		if err != nil {
			return 0, errors.Wrap(err, "failed to get scaled value or int from maxUnavailable")
		}

		return val, nil
	}

	return 0, nil
}

// SelectMachinesToDelete selects the machines to delete based on the machine state, desired replica count, and
// the DeletePolicy.
func (rollingUpdateStrategy rollingUpdateStrategy) SelectMachinesToDelete(ctx context.Context, desiredReplicaCount int32, machinesByProviderID map[string]infrav1exp.AzureMachinePoolMachine) ([]infrav1exp.AzureMachinePoolMachine, error) {
	ctx, _, done := tele.StartSpanWithLogger(
		ctx,
		"strategies.rollingUpdateStrategy.SelectMachinesToDelete",
	)
	defer done()

	maxUnavailable, err := rollingUpdateStrategy.maxUnavailable(int(desiredReplicaCount))
	if err != nil {
		return nil, err
	}

	var (
		order = func() func(machines []infrav1exp.AzureMachinePoolMachine) []infrav1exp.AzureMachinePoolMachine {
			switch rollingUpdateStrategy.DeletePolicy {
			case infrav1exp.OldestDeletePolicyType:
				return orderByOldest
			case infrav1exp.NewestDeletePolicyType:
				return orderByNewest
			default:
				return orderRandom
			}
		}()
		log                        = ctrl.LoggerFrom(ctx).V(4)
		failedMachines             = order(getFailedMachines(machinesByProviderID))
		deletingMachines           = order(getDeletingMachines(machinesByProviderID))
		readyMachines              = order(getReadyMachines(machinesByProviderID))
		machinesWithoutLatestModel = order(getMachinesWithoutLatestModel(machinesByProviderID))
		overProvisionCount         = len(readyMachines) - int(desiredReplicaCount)
		disruptionBudget           = func() int {
			if maxUnavailable > int(desiredReplicaCount) {
				return int(desiredReplicaCount)
			}

			return len(readyMachines) - int(desiredReplicaCount) + maxUnavailable
		}()
	)

	log.Info("selecting machines to delete",
		"readyMachines", len(readyMachines),
		"desiredReplicaCount", desiredReplicaCount,
		"maxUnavailable", maxUnavailable,
		"disruptionBudget", disruptionBudget,
		"machinesWithoutTheLatestModel", len(machinesWithoutLatestModel),
		"failedMachines", len(failedMachines),
	)

	// if we have failed or deleting machines, remove them
	if len(failedMachines) > 0 || len(deletingMachines) > 0 {
		log.Info("failed or deleting machines", "desiredReplicaCount", desiredReplicaCount, "maxUnavailable", maxUnavailable, "failedMachines", getProviderIDs(failedMachines), "deletingMachines", getProviderIDs(deletingMachines))
		return append(failedMachines, deletingMachines...), nil
	}

	// if we have deleting machines, remove them
	if len(failedMachines) > 0 {
		log.Info("failed machines", "desiredReplicaCount", desiredReplicaCount, "maxUnavailable", maxUnavailable, "failedMachines", getProviderIDs(failedMachines))
		return failedMachines, nil
	}

	// if we have not yet reached our desired count, don't try to delete anything but failed machines
	if len(readyMachines) < int(desiredReplicaCount) {
		log.Info("not enough ready machines", "desiredReplicaCount", desiredReplicaCount, "readyMachinesCount", len(readyMachines), "machinesByProviderID", len(machinesByProviderID))
		return []infrav1exp.AzureMachinePoolMachine{}, nil
	}

	// we have too many machines, let's choose the oldest to remove
	if overProvisionCount > 0 {
		var toDelete []infrav1exp.AzureMachinePoolMachine
		log.Info("over-provisioned", "desiredReplicaCount", desiredReplicaCount, "overProvisionCount", overProvisionCount, "machinesWithoutLatestModel", getProviderIDs(machinesWithoutLatestModel))
		// we are over-provisioned try to remove old models
		for _, v := range machinesWithoutLatestModel {
			if len(toDelete) >= overProvisionCount {
				return toDelete, nil
			}

			toDelete = append(toDelete, v)
		}

		log.Info("over-provisioned ready", "desiredReplicaCount", desiredReplicaCount, "overProvisionCount", overProvisionCount, "readyMachines", getProviderIDs(readyMachines))
		// remove ready machines
		for _, v := range readyMachines {
			if len(toDelete) >= overProvisionCount {
				return toDelete, nil
			}

			toDelete = append(toDelete, v)
		}

		return toDelete, nil
	}

	if len(machinesWithoutLatestModel) <= 0 {
		log.Info("nothing more to do since all the AzureMachinePoolMachine(s) are the latest model and not over-provisioned")
		return []infrav1exp.AzureMachinePoolMachine{}, nil
	}

	if disruptionBudget <= 0 {
		log.Info("exit early since disruption budget is less than or equal to zero", "disruptionBudget", disruptionBudget, "desiredReplicaCount", desiredReplicaCount, "maxUnavailable", maxUnavailable, "readyMachines", getProviderIDs(readyMachines), "readyMachinesCount", len(readyMachines))
		return []infrav1exp.AzureMachinePoolMachine{}, nil
	}

	var toDelete []infrav1exp.AzureMachinePoolMachine
	log.Info("removing ready machines within disruption budget", "desiredReplicaCount", desiredReplicaCount, "maxUnavailable", maxUnavailable, "readyMachines", getProviderIDs(readyMachines), "readyMachinesCount", len(readyMachines))
	for _, v := range readyMachines {
		if len(toDelete) >= disruptionBudget {
			return toDelete, nil
		}

		if !v.Status.LatestModelApplied {
			toDelete = append(toDelete, v)
		}
	}

	log.Info("completed without filling toDelete", "toDelete", getProviderIDs(toDelete), "numToDelete", len(toDelete))
	return toDelete, nil
}

func getFailedMachines(machinesByProviderID map[string]infrav1exp.AzureMachinePoolMachine) []infrav1exp.AzureMachinePoolMachine {
	var machines []infrav1exp.AzureMachinePoolMachine
	for _, v := range machinesByProviderID {
		// ready status, with provisioning state Succeeded, and not marked for delete
		if v.Status.ProvisioningState != nil && *v.Status.ProvisioningState == infrav1.Failed {
			machines = append(machines, v)
		}
	}

	return machines
}

func getDeletingMachines(machinesByProviderID map[string]infrav1exp.AzureMachinePoolMachine) []infrav1exp.AzureMachinePoolMachine {
	var machines []infrav1exp.AzureMachinePoolMachine
	for _, v := range machinesByProviderID {
		// ready status, with provisioning state Succeeded, and not marked for delete
		if v.Status.ProvisioningState != nil && *v.Status.ProvisioningState == infrav1.Deleting {
			machines = append(machines, v)
		}
	}

	return machines
}

func getReadyMachines(machinesByProviderID map[string]infrav1exp.AzureMachinePoolMachine) []infrav1exp.AzureMachinePoolMachine {
	var readyMachines []infrav1exp.AzureMachinePoolMachine
	for _, v := range machinesByProviderID {
		// ready status, with provisioning state Succeeded, and not marked for delete
		if v.Status.Ready && v.Status.ProvisioningState != nil && *v.Status.ProvisioningState == infrav1.Succeeded {
			readyMachines = append(readyMachines, v)
		}
	}

	return readyMachines
}

func getMachinesWithoutLatestModel(machinesByProviderID map[string]infrav1exp.AzureMachinePoolMachine) []infrav1exp.AzureMachinePoolMachine {
	var machinesWithLatestModel []infrav1exp.AzureMachinePoolMachine
	for _, v := range machinesByProviderID {
		if !v.Status.LatestModelApplied {
			machinesWithLatestModel = append(machinesWithLatestModel, v)
		}
	}

	return machinesWithLatestModel
}

func orderByNewest(machines []infrav1exp.AzureMachinePoolMachine) []infrav1exp.AzureMachinePoolMachine {
	sort.Slice(machines, func(i, j int) bool {
		return machines[i].ObjectMeta.CreationTimestamp.After(machines[j].ObjectMeta.CreationTimestamp.Time)
	})
	sort.SliceStable(machines, func(i, j int) bool {
		return compareDeleteAnnotations(machines[i], machines[j])
	})
	return machines
}

func orderByOldest(machines []infrav1exp.AzureMachinePoolMachine) []infrav1exp.AzureMachinePoolMachine {
	sort.Slice(machines, func(i, j int) bool {
		return machines[j].ObjectMeta.CreationTimestamp.After(machines[i].ObjectMeta.CreationTimestamp.Time)
	})
	sort.SliceStable(machines, func(i, j int) bool {
		return compareDeleteAnnotations(machines[i], machines[j])
	})
	return machines
}

func orderRandom(machines []infrav1exp.AzureMachinePoolMachine) []infrav1exp.AzureMachinePoolMachine {
	rand.Seed(time.Now().UnixNano())
	rand.Shuffle(len(machines), func(i, j int) { machines[i], machines[j] = machines[j], machines[i] })
	sort.SliceStable(machines, func(i, j int) bool {
		return compareDeleteAnnotations(machines[i], machines[j])
	})
	return machines
}

// compareDeleteAnnotations tries to parse the "cluster.x-k8s.io/delete-machine" annotation on a machine as
// a date value in the format that cluster-autoscaler uses. It falls back to string comparison if that fails,
// with "false" and "" (empty string) sorted last.
//
// To annotate a machine for deletion manually, run this command on the management cluster:
//   kubectl annotate machine <machine-name> cluster.x-k8s.io/delete-machine="$(date +"%Y-%d-%m %T %z %Z")"
// Then scale down the machine pool to delete the machine.
//   kubectl scale machine-pool/<machinepool-name> --replicas=<N-1>
func compareDeleteAnnotations(i, j infrav1exp.AzureMachinePoolMachine) bool {
	ia := deleteMachineAnnotation(i)
	ja := deleteMachineAnnotation(j)
	layout := "2006-01-02 15:04:05.999999999 -0700 MST"
	iTime, iErr := time.Parse(layout, strings.Split(ia, " m=")[0])
	jTime, jErr := time.Parse(layout, strings.Split(ja, " m=")[0])
	if iErr == nil && jErr == nil {
		return iTime.Before(jTime)
	}
	if iErr != nil && jErr == nil {
		return false
	}
	if iErr == nil && jErr != nil {
		return true
	}
	return ja == "false" || ja == ""
}

func deleteMachineAnnotation(machine infrav1exp.AzureMachinePoolMachine) string {
	if machine.ObjectMeta.Annotations != nil {
		if val, ok := machine.ObjectMeta.Annotations[clusterv1.DeleteMachineAnnotation]; ok {
			return val
		}
	}
	return ""
}

func getProviderIDs(machines []infrav1exp.AzureMachinePoolMachine) []string {
	ids := make([]string, len(machines))
	for i, machine := range machines {
		ids[i] = machine.Spec.ProviderID
	}

	return ids
}
