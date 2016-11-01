// Copyright (c) 2016 Tigera, Inc. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package calc

import (
	"fmt"
	log "github.com/Sirupsen/logrus"
	"github.com/projectcalico/libcalico-go/lib/backend/api"
	"github.com/projectcalico/libcalico-go/lib/backend/model"
	"sort"
)

type PolicySorter struct {
	tier *tierInfo
}

func NewPolicySorter() *PolicySorter {
	return &PolicySorter{
		tier: &tierInfo{

			Name:     "default",
			Policies: make(map[model.PolicyKey]*model.Policy),
		},
	}
}

func (poc *PolicySorter) OnUpdate(update api.Update) (dirty bool) {
	switch key := update.Key.(type) {
	case model.PolicyKey:
		oldPolicy := poc.tier.Policies[key]
		if update.Value != nil {
			newPolicy := update.Value.(*model.Policy)
			if oldPolicy == nil || oldPolicy.Order != newPolicy.Order {
				dirty = true
			}
			poc.tier.Policies[key] = newPolicy
		} else {
			if oldPolicy != nil {
				delete(poc.tier.Policies, key)
				dirty = true
			}
		}
	}
	return
}

func (poc *PolicySorter) Sorted() *tierInfo {
	tierInfo := poc.tier
	tierInfo.OrderedPolicies = make([]PolKV, 0, len(tierInfo.Policies))
	for k, v := range tierInfo.Policies {
		tierInfo.OrderedPolicies = append(tierInfo.OrderedPolicies,
			PolKV{Key: k, Value: v})
	}
	if log.GetLevel() >= log.DebugLevel {
		names := make([]string, len(tierInfo.OrderedPolicies))
		for ii, kv := range tierInfo.OrderedPolicies {
			names[ii] = fmt.Sprintf("%v(%v)",
				kv.Key.Name, *kv.Value.Order)
		}
		log.Infof("Before sorting policies: %v", names)
	}
	sort.Sort(PolicyByOrder(tierInfo.OrderedPolicies))
	if log.GetLevel() >= log.DebugLevel {
		names := make([]string, len(tierInfo.OrderedPolicies))
		for ii, kv := range tierInfo.OrderedPolicies {
			names[ii] = fmt.Sprintf("%v(%v)",
				kv.Key.Name, *kv.Value.Order)
		}
		log.Infof("After sorting policies: %v", names)
	}
	return tierInfo
}

type PolKV struct {
	Key   model.PolicyKey
	Value *model.Policy
}

type PolicyByOrder []PolKV

func (a PolicyByOrder) Len() int      { return len(a) }
func (a PolicyByOrder) Swap(i, j int) { a[i], a[j] = a[j], a[i] }
func (a PolicyByOrder) Less(i, j int) bool {
	bothNil := a[i].Value.Order == nil && a[j].Value.Order == nil
	bothSet := a[i].Value.Order != nil && a[j].Value.Order != nil
	ordersEqual := bothNil || bothSet && (*a[i].Value.Order == *a[j].Value.Order)

	if ordersEqual {
		// Use name as tie-break.
		result := a[i].Key.Name < a[j].Key.Name
		return result
	}

	// nil order maps to "infinity"
	if a[i].Value.Order == nil {
		return false
	} else if a[j].Value.Order == nil {
		return true
	}

	// Otherwise, use numeric comparison.
	return *a[i].Value.Order < *a[j].Value.Order
}

type tierInfo struct {
	Name            string
	Valid           bool
	Order           *float64
	Policies        map[model.PolicyKey]*model.Policy
	OrderedPolicies []PolKV
}

func NewTierInfo(name string) *tierInfo {
	return &tierInfo{
		Name:     name,
		Policies: make(map[model.PolicyKey]*model.Policy),
	}
}

func (t tierInfo) String() string {
	policies := make([]string, len(t.OrderedPolicies))
	for ii, pol := range t.OrderedPolicies {
		policies[ii] = pol.Key.Name
	}
	return fmt.Sprintf("%v -> %v", t.Name, policies)
}
