/*
Copyright 2022 The Kubernetes Authors.

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

package provider

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/Azure/azure-sdk-for-go/services/compute/mgmt/2022-08-01/compute"

	"k8s.io/apimachinery/pkg/types"
	cloudprovider "k8s.io/cloud-provider"
	"k8s.io/klog/v2"
	"k8s.io/utils/pointer"

	azcache "sigs.k8s.io/cloud-provider-azure/pkg/cache"
	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
)

func (fs *FlexScaleSet) newVmssFlexCache() (azcache.Resource, error) {
	getter := func(ctx context.Context, _ string) (interface{}, error) {
		localCache := &sync.Map{}

		allResourceGroups, err := fs.GetResourceGroups()
		if err != nil {
			return nil, err
		}

		for _, resourceGroup := range allResourceGroups.UnsortedList() {
			allScaleSets, rerr := fs.VirtualMachineScaleSetsClient.List(ctx, resourceGroup)
			if rerr != nil {
				if rerr.IsNotFound() {
					klog.Warningf("Skip caching vmss for resource group %s due to error: %v", resourceGroup, rerr.Error())
					continue
				}
				klog.Errorf("VirtualMachineScaleSetsClient.List failed: %v", rerr)
				return nil, rerr.Error()
			}

			for i := range allScaleSets {
				scaleSet := allScaleSets[i]
				if scaleSet.ID == nil || *scaleSet.ID == "" {
					klog.Warning("failed to get the ID of VMSS Flex")
					continue
				}

				if scaleSet.OrchestrationMode == compute.Flexible {
					localCache.Store(*scaleSet.ID, &scaleSet)
				}
			}
		}

		return localCache, nil
	}

	if fs.Config.VmssFlexCacheTTLInSeconds == 0 {
		fs.Config.VmssFlexCacheTTLInSeconds = consts.VmssFlexCacheTTLDefaultInSeconds
	}
	return azcache.NewTimedCache(time.Duration(fs.Config.VmssFlexCacheTTLInSeconds)*time.Second, getter, fs.Cloud.Config.DisableAPICallCache)
}

func (fs *FlexScaleSet) newVmssFlexVMCache() (azcache.Resource, error) {
	getter := func(ctx context.Context, key string) (interface{}, error) {
		localCache := &sync.Map{}

		vms, rerr := fs.VirtualMachinesClient.ListVmssFlexVMsWithoutInstanceView(ctx, key)
		if rerr != nil {
			klog.Errorf("ListVmssFlexVMsWithoutInstanceView failed: %v", rerr)
			return nil, rerr.Error()
		}

		for i := range vms {
			vm := vms[i]
			if vm.OsProfile != nil && vm.OsProfile.ComputerName != nil {
				localCache.Store(strings.ToLower(*vm.OsProfile.ComputerName), &vm)
			}
			fs.cacheVirtualMachine(ctx, vm)
		}

		vms, rerr = fs.VirtualMachinesClient.ListVmssFlexVMsWithOnlyInstanceView(ctx, key)
		if rerr != nil {
			klog.Errorf("ListVmssFlexVMsWithOnlyInstanceView failed: %v", rerr)
			return nil, rerr.Error()
		}

		for i := range vms {
			vm := vms[i]
			if vm.Name != nil {
				nodeName, ok := fs.vmssFlexVMNameToNodeName.Load(*vm.Name)
				if !ok {
					continue
				}

				cached, ok := localCache.Load(nodeName)
				if ok {
					cachedVM := cached.(*compute.VirtualMachine)
					cachedVM.VirtualMachineProperties.InstanceView = vm.VirtualMachineProperties.InstanceView
				}
			}
		}

		return localCache, nil
	}

	if fs.Config.VmssFlexVMCacheTTLInSeconds == 0 {
		fs.Config.VmssFlexVMCacheTTLInSeconds = consts.VmssFlexVMCacheTTLDefaultInSeconds
	}
	return azcache.NewTimedCache(time.Duration(fs.Config.VmssFlexVMCacheTTLInSeconds)*time.Second, getter, fs.Cloud.Config.DisableAPICallCache)
}

func (fs *FlexScaleSet) getNodeNameByVMName(ctx context.Context, vmName string) (string, error) {
	fs.lockMap.LockEntry(consts.GetNodeVmssFlexIDLockKey)
	defer fs.lockMap.UnlockEntry(consts.GetNodeVmssFlexIDLockKey)
	cachedNodeName, isCached := fs.vmssFlexVMNameToNodeName.Load(vmName)
	if isCached {
		return fmt.Sprintf("%v", cachedNodeName), nil
	}

	getter := func(ctx context.Context, vmName string, crt azcache.AzureCacheReadType) (string, error) {
		vm, err := fs.getVmssFlexVMByVMName(ctx, vmName, crt)
		if err != nil {
			return "", err
		}

		if vm.OsProfile != nil && vm.OsProfile.ComputerName != nil {
			return strings.ToLower(*vm.OsProfile.ComputerName), nil
		}

		return "", cloudprovider.InstanceNotFound
	}

	nodeName, err := getter(ctx, vmName, azcache.CacheReadTypeDefault)
	if errors.Is(err, cloudprovider.InstanceNotFound) {
		klog.V(2).Infof("Could not find node (%s) in the existing cache. Forcely freshing the cache to check again...", nodeName)
		return getter(ctx, vmName, azcache.CacheReadTypeForceRefresh)
	}
	return nodeName, err

}

func (fs *FlexScaleSet) getNodeVmssFlexID(ctx context.Context, nodeName string) (string, error) {
	fs.lockMap.LockEntry(consts.GetNodeVmssFlexIDLockKey)
	defer fs.lockMap.UnlockEntry(consts.GetNodeVmssFlexIDLockKey)
	cachedVmssFlexID, isCached := fs.vmssFlexNodeNameToVmssID.Load(nodeName)

	if isCached {
		return fmt.Sprintf("%v", cachedVmssFlexID), nil
	}

	getter := func(ctx context.Context, nodeName string, crt azcache.AzureCacheReadType) (string, error) {
		cached, err := fs.vmssFlexCache.Get(ctx, consts.VmssFlexKey, crt)
		if err != nil {
			return "", err
		}
		vmssFlexes := cached.(*sync.Map)

		var vmssFlexIDs []string
		vmssFlexes.Range(func(key, value interface{}) bool {
			vmssFlexID := key.(string)
			vmssFlex := value.(*compute.VirtualMachineScaleSet)
			vmssPrefix := pointer.StringDeref(vmssFlex.Name, "")
			if vmssFlex.VirtualMachineProfile != nil &&
				vmssFlex.VirtualMachineProfile.OsProfile != nil &&
				vmssFlex.VirtualMachineProfile.OsProfile.ComputerNamePrefix != nil {
				vmssPrefix = pointer.StringDeref(vmssFlex.VirtualMachineProfile.OsProfile.ComputerNamePrefix, "")
			}
			if strings.EqualFold(vmssPrefix, nodeName[:len(nodeName)-6]) {
				// we should check this vmss first since nodeName and vmssFlex.Name or
				// ComputerNamePrefix belongs to same vmss, so prepend here
				vmssFlexIDs = append([]string{vmssFlexID}, vmssFlexIDs...)
			} else {
				vmssFlexIDs = append(vmssFlexIDs, vmssFlexID)
			}
			return true
		})

		for _, vmssID := range vmssFlexIDs {
			if _, err := fs.vmssFlexVMCache.Get(ctx, vmssID, azcache.CacheReadTypeForceRefresh); err != nil {
				klog.Errorf("failed to refresh vmss flex VM cache for vmssFlexID %s", vmssID)
				return "", err
			}
			// if the vm is cached stop refreshing
			cachedVmssFlexID, isCached = fs.vmssFlexNodeNameToVmssID.Load(nodeName)
			if isCached {
				return fmt.Sprintf("%v", cachedVmssFlexID), nil
			}
		}
		return "", cloudprovider.InstanceNotFound
	}

	vmssFlexID, err := getter(ctx, nodeName, azcache.CacheReadTypeDefault)
	if errors.Is(err, cloudprovider.InstanceNotFound) {
		klog.V(2).Infof("Could not find node (%s) in the existing cache. Forcely freshing the cache to check again...", nodeName)
		return getter(ctx, nodeName, azcache.CacheReadTypeForceRefresh)
	}
	return vmssFlexID, err

}

func (fs *FlexScaleSet) getVmssFlexVM(ctx context.Context, nodeName string, crt azcache.AzureCacheReadType) (vm compute.VirtualMachine, err error) {
	cachedVMName, isCached := fs.vmssFlexNodeNameToVMName.Load(nodeName)
	if isCached {
		return fs.getVmssFlexVMByVMName(ctx, cachedVMName.(string), crt)
	}

	vmssFlexID, err := fs.getNodeVmssFlexID(ctx, nodeName)
	if err != nil {
		return vm, err
	}

	cached, err := fs.vmssFlexVMCache.Get(ctx, vmssFlexID, crt)
	if err != nil {
		return vm, err
	}
	vmMap := cached.(*sync.Map)
	cachedVM, ok := vmMap.Load(nodeName)
	if !ok {
		klog.V(2).Infof("did not find node (%s) in the existing cache, which means it is deleted...", nodeName)
		return vm, cloudprovider.InstanceNotFound
	}

	return *(cachedVM.(*compute.VirtualMachine)), nil
}

func (fs *FlexScaleSet) getVmssFlexByVmssFlexID(ctx context.Context, vmssFlexID string, crt azcache.AzureCacheReadType) (*compute.VirtualMachineScaleSet, error) {
	cached, err := fs.vmssFlexCache.Get(ctx, consts.VmssFlexKey, crt)
	if err != nil {
		return nil, err
	}
	vmssFlexes := cached.(*sync.Map)
	if vmssFlex, ok := vmssFlexes.Load(vmssFlexID); ok {
		result := vmssFlex.(*compute.VirtualMachineScaleSet)
		return result, nil
	}

	klog.V(2).Infof("Couldn't find VMSS Flex with ID %s, refreshing the cache", vmssFlexID)
	cached, err = fs.vmssFlexCache.Get(ctx, consts.VmssFlexKey, azcache.CacheReadTypeForceRefresh)
	if err != nil {
		return nil, err
	}
	vmssFlexes = cached.(*sync.Map)
	if vmssFlex, ok := vmssFlexes.Load(vmssFlexID); ok {
		result := vmssFlex.(*compute.VirtualMachineScaleSet)
		return result, nil
	}
	return nil, cloudprovider.InstanceNotFound
}

func (fs *FlexScaleSet) getVmssFlexByNodeName(ctx context.Context, nodeName string, crt azcache.AzureCacheReadType) (*compute.VirtualMachineScaleSet, error) {
	vmssFlexID, err := fs.getNodeVmssFlexID(ctx, nodeName)
	if err != nil {
		return nil, err
	}
	vmssFlex, err := fs.getVmssFlexByVmssFlexID(ctx, vmssFlexID, crt)
	if err != nil {
		return nil, err
	}
	return vmssFlex, nil
}

func (fs *FlexScaleSet) getVmssFlexIDByName(ctx context.Context, vmssFlexName string) (string, error) {
	cached, err := fs.vmssFlexCache.Get(ctx, consts.VmssFlexKey, azcache.CacheReadTypeDefault)
	if err != nil {
		return "", err
	}
	var targetVmssFlexID string
	vmssFlexes := cached.(*sync.Map)
	vmssFlexes.Range(func(key, _ interface{}) bool {
		vmssFlexID := key.(string)
		name, err := getLastSegment(vmssFlexID, "/")
		if err != nil {
			return true
		}
		if strings.EqualFold(name, vmssFlexName) {
			targetVmssFlexID = vmssFlexID
			return false
		}
		return true
	})
	if targetVmssFlexID != "" {
		return targetVmssFlexID, nil
	}
	return "", cloudprovider.InstanceNotFound
}

func (fs *FlexScaleSet) getVmssFlexByName(ctx context.Context, vmssFlexName string) (*compute.VirtualMachineScaleSet, error) {
	cached, err := fs.vmssFlexCache.Get(ctx, consts.VmssFlexKey, azcache.CacheReadTypeDefault)
	if err != nil {
		return nil, err
	}

	var targetVmssFlex *compute.VirtualMachineScaleSet
	vmssFlexes := cached.(*sync.Map)
	vmssFlexes.Range(func(key, value interface{}) bool {
		vmssFlexID := key.(string)
		vmssFlex := value.(*compute.VirtualMachineScaleSet)
		name, err := getLastSegment(vmssFlexID, "/")
		if err != nil {
			return true
		}
		if strings.EqualFold(name, vmssFlexName) {
			targetVmssFlex = vmssFlex
			return false
		}
		return true
	})
	if targetVmssFlex != nil {
		return targetVmssFlex, nil
	}
	return nil, cloudprovider.InstanceNotFound
}

func (fs *FlexScaleSet) getVmssFlexVMByVMName(ctx context.Context, vmName string, crt azcache.AzureCacheReadType) (compute.VirtualMachine, error) {
	vm, err := fs.getVirtualMachine(ctx, types.NodeName(vmName), crt)
	if err != nil {
		return compute.VirtualMachine{}, err
	}
	fs.cacheVirtualMachine(ctx, vm)
	return vm, nil
}

func (fs *FlexScaleSet) cacheVirtualMachine(ctx context.Context, vm compute.VirtualMachine) {
	if vm.OsProfile != nil && vm.OsProfile.ComputerName != nil {
		fs.vmssFlexVMNameToNodeName.Store(*vm.Name, strings.ToLower(*vm.OsProfile.ComputerName))
		fs.vmssFlexNodeNameToVMName.Store(strings.ToLower(*vm.OsProfile.ComputerName), *vm.Name)
		if vm.VirtualMachineScaleSet != nil && vm.VirtualMachineScaleSet.ID != nil {
			fs.vmssFlexNodeNameToVmssID.Store(strings.ToLower(*vm.OsProfile.ComputerName), *vm.VirtualMachineScaleSet.ID)
		}
	}
}

func (fs *FlexScaleSet) DeleteCacheForNode(ctx context.Context, nodeName string) error {
	if fs.Config.DisableAPICallCache {
		return nil
	}
	vmssFlexID, err := fs.getNodeVmssFlexID(ctx, nodeName)
	if err != nil {
		klog.Errorf("getNodeVmssFlexID(%s) failed with %v", nodeName, err)
		return err
	}

	fs.lockMap.LockEntry(vmssFlexID)
	defer fs.lockMap.UnlockEntry(vmssFlexID)
	cached, err := fs.vmssFlexVMCache.Get(ctx, vmssFlexID, azcache.CacheReadTypeNoRefresh)
	if err != nil {
		klog.Errorf("vmssFlexVMCache.Get(%s, %s) failed with %v", vmssFlexID, nodeName, err)
		return err
	}
	if cached != nil {
		vmMap := cached.(*sync.Map)
		vmMap.Delete(nodeName)
		fs.vmssFlexVMCache.Update(vmssFlexID, vmMap)
	}

	cachedVMName, isCached := fs.vmssFlexNodeNameToVMName.Load(nodeName)
	if isCached {
		vmName := cachedVMName.(string)
		fs.vmssFlexVMNameToNodeName.Delete(vmName)
	}

	fs.vmssFlexNodeNameToVmssID.Delete(nodeName)
	fs.vmssFlexNodeNameToVMName.Delete(nodeName)

	klog.V(2).Infof("DeleteCacheForNode(%s, %s) successfully", vmssFlexID, nodeName)
	return nil
}
