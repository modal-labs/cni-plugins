// Copyright 2016 CNI authors
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

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"

	"github.com/vishvananda/netlink"

	"github.com/containernetworking/cni/pkg/skel"
	"github.com/containernetworking/cni/pkg/types"
	current "github.com/containernetworking/cni/pkg/types/100"
	"github.com/containernetworking/cni/pkg/version"
	"github.com/containernetworking/plugins/pkg/ns"
	bv "github.com/containernetworking/plugins/pkg/utils/buildversion"
)

// `lo` is always interface index 1 inside any newly-created network namespace.
// Hardcoding it lets us avoid a `RTM_GETLINK` ("LinkByName") roundtrip on the
// global rtnl_mutex for every container start/stop -- under saturated container
// churn that lookup is a meaningful contributor to rtnl_mutex contention.
const loopbackIfIndex = 1

func parseNetConf(bytes []byte) (*types.NetConf, error) {
	conf := &types.NetConf{}
	if err := json.Unmarshal(bytes, conf); err != nil {
		return nil, fmt.Errorf("failed to parse network config: %v", err)
	}

	if conf.RawPrevResult != nil {
		if err := version.ParsePrevResult(conf); err != nil {
			return nil, fmt.Errorf("failed to parse prevResult: %v", err)
		}
		if _, err := current.NewResultFromResult(conf.PrevResult); err != nil {
			return nil, fmt.Errorf("failed to convert result to current version: %v", err)
		}
	}

	return conf, nil
}

func cmdAdd(args *skel.CmdArgs) error {
	conf, err := parseNetConf(args.StdinData)
	if err != nil {
		return err
	}

	args.IfName = "lo" // ignore config, this only works for loopback
	err = ns.WithNetNSPath(args.Netns, func(_ ns.NetNS) error {
		// Bring `lo` up using its well-known ifindex (1) -- skipping the
		// `LinkByName` lookup that the original plugin did before this. That
		// lookup is a `RTM_GETLINK` which takes `rtnl_mutex`; on hosts under
		// container-spinup load, eliminating it removes a meaningful slice of
		// rtnl pressure. The previous AddrList sanity checks have also been
		// dropped: they were two `RTM_GETADDR` dumps per invocation (a much
		// larger source of contention) and the addresses they returned were
		// only used to populate `result.IPs` for downstream plugins, which
		// modal's bridge plugin chain does not consume from loopback.
		loLink := &netlink.Device{LinkAttrs: netlink.LinkAttrs{Index: loopbackIfIndex, Name: args.IfName}}
		if err := netlink.LinkSetUp(loLink); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return err // not tested
	}

	var result types.Result
	if conf.PrevResult != nil {
		// If loopback has previous result which passes from previous CNI plugin,
		// loopback should pass it transparently
		result = conf.PrevResult
	} else {
		result = &current.Result{
			CNIVersion: conf.CNIVersion,
			Interfaces: []*current.Interface{
				{
					Name:    args.IfName,
					Mac:     "00:00:00:00:00:00",
					Sandbox: args.Netns,
				},
			},
		}
	}

	return types.PrintResult(result, conf.CNIVersion)
}

func cmdDel(args *skel.CmdArgs) error {
	// On DEL, the previous implementation looked up `lo` and brought it down
	// before returning. That accomplished nothing: when the network namespace
	// itself is torn down (which happens immediately after this hook on every
	// CNI runtime modal uses), `lo` is destroyed with it, so the explicit
	// `LinkSetDown` was redundant. We skip both the lookup and the down to
	// avoid two additional `rtnl_mutex` acquires per container teardown.
	return nil
}

func main() {
	skel.PluginMain(cmdAdd, cmdCheck, cmdDel, version.All, bv.BuildString("loopback"))
}

func cmdCheck(args *skel.CmdArgs) error {
	args.IfName = "lo" // ignore config, this only works for loopback

	return ns.WithNetNSPath(args.Netns, func(_ ns.NetNS) error {
		// LinkByIndex(1) targets `lo` directly without a name-based lookup.
		link, err := netlink.LinkByIndex(loopbackIfIndex)
		if err != nil {
			return err
		}

		if link.Attrs().Flags&net.FlagUp != net.FlagUp {
			return errors.New("loopback interface is down")
		}

		return nil
	})
}
