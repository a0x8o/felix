// Copyright (c) 2016 Tigera, Inc. All rights reserved.

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
	log "github.com/Sirupsen/logrus"
	"github.com/projectcalico/felix/go/felix/dispatcher"
	"github.com/projectcalico/felix/go/felix/ip"
	"github.com/projectcalico/felix/go/felix/labelindex"
	"github.com/projectcalico/felix/go/felix/tagindex"
	"github.com/projectcalico/libcalico-go/lib/backend/api"
	"github.com/projectcalico/libcalico-go/lib/backend/model"
	"github.com/projectcalico/libcalico-go/lib/hash"
	"github.com/projectcalico/libcalico-go/lib/net"
	"github.com/projectcalico/libcalico-go/lib/selector"
)

type ipSetUpdateCallbacks interface {
	OnIPSetAdded(setID string)
	OnIPAdded(setID string, ip ip.Addr)
	OnIPRemoved(setID string, ip ip.Addr)
	OnIPSetRemoved(setID string)
}

type rulesUpdateCallbacks interface {
	OnPolicyActive(model.PolicyKey, *ParsedRules)
	OnPolicyInactive(model.PolicyKey)
	OnProfileActive(model.ProfileRulesKey, *ParsedRules)
	OnProfileInactive(model.ProfileRulesKey)
}

type endpointCallbacks interface {
	OnEndpointTierUpdate(endpointKey model.Key,
		endpoint interface{},
		filteredTiers []tierInfo)
}

type configCallbacks interface {
	OnConfigUpdate(globalConfig, hostConfig map[string]string)
	OnDatastoreNotReady()
}

type passthruCallbacks interface {
	OnHostIPUpdate(hostname string, ip *net.IP)
	OnHostIPRemove(hostname string)
	OnIPPoolUpdate(model.IPPoolKey, *model.IPPool)
	OnIPPoolRemove(model.IPPoolKey)
}

type PipelineCallbacks interface {
	ipSetUpdateCallbacks
	rulesUpdateCallbacks
	endpointCallbacks
	configCallbacks
	passthruCallbacks
}

func NewCalculationGraph(callbacks PipelineCallbacks, hostname string) (allUpdDispatcher *dispatcher.Dispatcher) {
	log.Infof("Creating calculation graph, filtered to hostname %v", hostname)
	// The source of the processing graph, this dispatcher will be fed all
	// the updates from the datastore, fanning them out to the registered
	// handlers.
	allUpdDispatcher = dispatcher.NewDispatcher()

	// Some of the handlers only need to know about local endpoints.
	// Create a second dispatcher which will filter out non-local endpoints.
	localEndpointDispatcher := dispatcher.NewDispatcher()
	(*localEndpointDispatcherReg)(localEndpointDispatcher).RegisterWith(allUpdDispatcher)
	localEndpointFilter := &endpointHostnameFilter{hostname: hostname}
	localEndpointFilter.RegisterWith(localEndpointDispatcher)

	// The active rules calculator matches local endpoints against policies
	// and profiles to figure out which policies/profiles are active on this
	// host.
	activeRulesCalc := NewActiveRulesCalculator()
	activeRulesCalc.RegisterWith(localEndpointDispatcher, allUpdDispatcher)

	// The rule scanner takes the output from the active rules calculator
	// and scans the individual rules for selectors and tags.  It generates
	// events when a new selector/tag starts/stops being used.
	ruleScanner := NewRuleScanner()
	activeRulesCalc.RuleScanner = ruleScanner
	ruleScanner.RulesUpdateCallbacks = callbacks

	// The active selector index matches the active selectors found by the
	// rule scanner against *all* endpoints.  It emits events when an
	// endpoint starts/stops matching one of the active selectors.  We
	// send the events to the membership calculator, which will extract the
	// ip addresses of the endpoints.  The member calculator handles tags
	// and selectors uniformly but we need to shim the interface because
	// it expects a string ID.
	var memberCalc *MemberCalculator
	activeSelectorIndex := labelindex.NewInheritIndex(
		func(selId, labelId interface{}) {
			// Match started callback.
			memberCalc.MatchStarted(labelId.(model.Key), selId.(string))
		},
		func(selId, labelId interface{}) {
			// Match stopped callback.
			memberCalc.MatchStopped(labelId.(model.Key), selId.(string))
		},
	)
	ruleScanner.OnSelectorActive = func(sel selector.Selector) {
		log.Infof("Selector %v now active", sel)
		callbacks.OnIPSetAdded(sel.UniqueId())
		activeSelectorIndex.UpdateSelector(sel.UniqueId(), sel)
	}
	ruleScanner.OnSelectorInactive = func(sel selector.Selector) {
		log.Infof("Selector %v now inactive", sel)
		activeSelectorIndex.DeleteSelector(sel.UniqueId())
		callbacks.OnIPSetRemoved(sel.UniqueId())
	}
	activeSelectorIndex.RegisterWith(allUpdDispatcher)

	// The active tag index does the same for tags.  Calculating which
	// endpoints match each tag.
	tagIndex := tagindex.NewIndex(
		func(key model.Key, tagID string) {
			memberCalc.MatchStarted(key, TagIPSetID(tagID))
		},
		func(key model.Key, tagID string) {
			memberCalc.MatchStopped(key, TagIPSetID(tagID))
		},
	)

	ruleScanner.OnTagActive = func(tag string) {
		log.Infof("Tag %v now active", tag)
		callbacks.OnIPSetAdded(hash.MakeUniqueID("t", tag))
		tagIndex.SetTagActive(tag)
	}
	ruleScanner.OnTagInactive = func(tag string) {
		log.Infof("Tag %v now inactive", tag)
		tagIndex.SetTagInactive(tag)
		callbacks.OnIPSetRemoved(hash.MakeUniqueID("t", tag))
	}
	tagIndex.RegisterWith(allUpdDispatcher)

	// The member calculator merges the IPs from different endpoints to
	// calculate the actual IPs that should be in each IP set.  It deals
	// with corner cases, such as having the same IP on multiple endpoints.
	memberCalc = NewMemberCalculator()
	// It needs to know about *all* endpoints to do the calculation.
	memberCalc.RegisterWith(allUpdDispatcher)
	// Hook it up to the output.
	memberCalc.callbacks = callbacks

	// The endpoint policy resolver marries up the active policies with
	// local endpoints and calculates the complete, ordered set of
	// policies that apply to each endpoint.
	polResolver := NewPolicyResolver()
	// Hook up the inputs to the policy resolver.
	activeRulesCalc.PolicyMatchListener = polResolver
	polResolver.RegisterWith(allUpdDispatcher, localEndpointDispatcher)

	// And hook its output to the callbacks.
	polResolver.Callbacks = callbacks

	// Register for host IP updates.
	hostIPPassthru := NewDataplanePassthru(callbacks)
	hostIPPassthru.RegisterWith(allUpdDispatcher)

	// Register for config updates.
	configBatcher := NewConfigBatcher(hostname, callbacks)
	configBatcher.RegisterWith(allUpdDispatcher)

	return allUpdDispatcher
}

type localEndpointDispatcherReg dispatcher.Dispatcher

func (l *localEndpointDispatcherReg) RegisterWith(disp *dispatcher.Dispatcher) {
	led := (*dispatcher.Dispatcher)(l)
	disp.Register(model.WorkloadEndpointKey{}, led.OnUpdate)
	disp.Register(model.HostEndpointKey{}, led.OnUpdate)
	disp.RegisterStatusHandler(led.OnDatamodelStatus)
}

type DataplanePassthru struct {
	callbacks passthruCallbacks
}

func NewDataplanePassthru(callbacks passthruCallbacks) *DataplanePassthru {
	return &DataplanePassthru{callbacks: callbacks}
}

func (h *DataplanePassthru) RegisterWith(dispatcher *dispatcher.Dispatcher) {
	dispatcher.Register(model.HostIPKey{}, h.OnUpdate)
	dispatcher.Register(model.IPPoolKey{}, h.OnUpdate)
}

func (h *DataplanePassthru) OnUpdate(update api.Update) (filterOut bool) {
	switch key := update.Key.(type) {
	case model.HostIPKey:
		hostname := key.Hostname
		if update.Value == nil {
			log.WithField("update", update).Debug("Passing-through HostIP deletion")
			h.callbacks.OnHostIPRemove(hostname)
		} else {
			log.WithField("update", update).Debug("Passing-through HostIP update")
			ip := update.Value.(*net.IP)
			h.callbacks.OnHostIPUpdate(hostname, ip)
		}
	case model.IPPoolKey:
		if update.Value == nil {
			log.WithField("update", update).Debug("Passing-through IPPool deletion")
			h.callbacks.OnIPPoolRemove(key)
		} else {
			log.WithField("update", update).Debug("Passing-through IPPool update")
			pool := update.Value.(*model.IPPool)
			h.callbacks.OnIPPoolUpdate(key, pool)
		}
	}

	return false
}

func TagIPSetID(tagID string) string {
	return hash.MakeUniqueID("t", tagID)
}

// endpointHostnameFilter provides an UpdateHandler that filters out endpoints
// that are not on the given host.
type endpointHostnameFilter struct {
	hostname string
}

func (f *endpointHostnameFilter) RegisterWith(localEndpointDisp *dispatcher.Dispatcher) {
	localEndpointDisp.Register(model.WorkloadEndpointKey{}, f.OnUpdate)
	localEndpointDisp.Register(model.HostEndpointKey{}, f.OnUpdate)
}

func (f *endpointHostnameFilter) OnUpdate(update api.Update) (filterOut bool) {
	switch key := update.Key.(type) {
	case model.WorkloadEndpointKey:
		if key.Hostname != f.hostname {
			filterOut = true
		}
	case model.HostEndpointKey:
		if key.Hostname != f.hostname {
			filterOut = true
		}
	}
	if !filterOut {
		// To keep log spam down, log only for local endpoints.
		if update.Value == nil {
			log.WithField("id", update.Key).Info("Local endpoint deleted")
		} else {
			log.WithField("id", update.Key).Info("Local endpoint updated")
		}
	}
	return
}
