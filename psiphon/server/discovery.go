/*
 * Copyright (c) 2024, Psiphon Inc.
 * All rights reserved.
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 *
 */

package server

import (
	"context"
	"net"
	"sync"

	"github.com/Psiphon-Labs/psiphon-tunnel-core/psiphon/common/errors"
	"github.com/Psiphon-Labs/psiphon-tunnel-core/psiphon/common/parameters"
	"github.com/Psiphon-Labs/psiphon-tunnel-core/psiphon/server/discovery"
)

const (
	DISCOVERY_STRATEGY_CLASSIC    = "classic"
	DISCOVERY_STRATEGY_CONSISTENT = "consistent"
)

// Discovery handles the discovery step of the "handshake" API request. It's
// safe for concurrent usage.
type Discovery struct {
	support         *SupportServices
	currentStrategy string
	discovery       *discovery.Discovery
	cancelFunc      context.CancelFunc

	sync.RWMutex
}

func newDiscovery(support *SupportServices) (*Discovery, error) {

	d := &Discovery{
		support: support,
	}

	err := d.reload(false)
	if err != nil {
		return nil, errors.Trace(err)
	}

	return d, nil
}

// reload reinitializes the underlying discovery component. If reloadedTactics
// is set and the target discovery strategy has not changed, then the
// underlying discovery component is not reinitialized.
func (d *Discovery) reload(reloadedTactics bool) error {

	// Determine which discovery strategy to use

	p, err := d.support.ServerTacticsParametersCache.Get(NewGeoIPData())
	if err != nil {
		return errors.Trace(err)
	}

	strategy := ""
	if !p.IsNil() {
		strategy = p.String(parameters.ServerDiscoveryStrategy)
	}
	if strategy == "" {
		// No tactics are configured; default to classic discovery.
		strategy = DISCOVERY_STRATEGY_CLASSIC
	}

	// Do not reinitialize underlying discovery component if only tactics have
	// been reloaded and the discovery strategy has not changed.
	if reloadedTactics && d.support.discovery.currentStrategy == strategy {
		return nil
	}

	// Initialize new discovery strategy.
	// TODO: do not reinitialize discovery if the discovery strategy and
	// discovery servers have not changed.
	var discoveryStrategy discovery.DiscoveryStrategy
	if strategy == DISCOVERY_STRATEGY_CONSISTENT {
		discoveryStrategy, err = discovery.NewConsistentHashingDiscovery()
		if err != nil {
			return errors.Trace(err)
		}
	} else if strategy == DISCOVERY_STRATEGY_CLASSIC {
		discoveryStrategy, err = discovery.NewClassicDiscovery(
			d.support.Config.DiscoveryValueHMACKey,
			discovery.RealClock{})
		if err != nil {
			return errors.Trace(err)
		}
	} else {
		return errors.Tracef("unknown strategy %s", strategy)
	}

	// Initialize and set underlying discovery component. Replaces old
	// component if discovery is already initialized.

	ctx, cancelFunc := context.WithCancel(context.Background())

	discovery, err := discovery.NewDiscovery(
		ctx,
		discovery.RealClock{},
		d.support.PsinetDatabase.GetDiscoveryServers(),
		discoveryStrategy)
	if err != nil {
		cancelFunc()
		return errors.Trace(err)
	}

	d.Lock()

	// Ensure resources used by previous underlying discovery component are
	// cleaned up.
	// Note: a more efficient impementation would not reinitialize the underlying
	// discovery entirely if the discovery strategy has not changed, but
	// instead would update the underlying set of discovery servers if the set
	// of discovery servers has changed.
	if d.cancelFunc != nil {
		d.cancelFunc()
	}

	d.discovery = discovery
	d.cancelFunc = cancelFunc
	d.currentStrategy = strategy

	d.Unlock()

	log.WithTraceFields(
		LogFields{"event_name": "discovery_strategy"}).Infof("using %s discovery", strategy)

	return nil
}

// DiscoverServers selects new encoded server entries to be "discovered" by
// the client, using the client's IP address as the input into the discovery
// algorithm.
func (d *Discovery) DiscoverServers(clientIP net.IP) []string {

	d.RLock()
	defer d.RUnlock()

	servers := d.discovery.SelectServers(clientIP)

	encodedServerEntries := make([]string, 0)

	for _, server := range servers {
		encodedServerEntries = append(encodedServerEntries, server.EncodedServerEntry)
	}

	return encodedServerEntries
}
