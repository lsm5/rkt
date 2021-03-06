// Copyright 2015 CoreOS, Inc.
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

package networking

import (
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"syscall"

	"github.com/coreos/rocket/Godeps/_workspace/src/github.com/appc/spec/schema/types"
	"github.com/coreos/rocket/Godeps/_workspace/src/github.com/vishvananda/netlink"

	"github.com/coreos/rocket/networking/netinfo"
	"github.com/coreos/rocket/networking/util"
)

const (
	ifnamePattern = "eth%d"
	selfNetNS     = "/proc/self/ns/net"
)

type activeNet struct {
	Net
	ifName string
	ip     net.IP
}

// "base" struct that's populated from the beginning
// describing the environment in which the container
// is running in
type containerEnv struct {
	rktRoot string
	contID  types.UUID
}

// Networking describes the networking details of a container.
type Networking struct {
	containerEnv

	MetadataIP net.IP

	contID     types.UUID
	hostNS     *os.File
	contNS     *os.File
	contNSPath string
	nets       []activeNet
}

// Setup produces a Networking object for a given container ID.
func Setup(rktRoot string, contID types.UUID) (*Networking, error) {
	var err error
	n := Networking{
		containerEnv: containerEnv{
			rktRoot: rktRoot,
			contID:  contID,
		},
	}

	defer func() {
		// cleanup on error
		if err != nil {
			n.Teardown()
		}
	}()

	if n.hostNS, n.contNS, err = basicNetNS(); err != nil {
		return nil, err
	}
	// we're in contNS!

	n.contNSPath = filepath.Join(rktRoot, "netns")
	if err = bindMountFile(selfNetNS, n.contNSPath); err != nil {
		return nil, err
	}

	nets, err := n.loadNets()
	if err != nil {
		return nil, fmt.Errorf("error loading network definitions: %v", err)
	}

	err = withNetNS(n.contNS, n.hostNS, func() error {
		n.nets, err = n.setupNets(n.contNSPath, nets)
		return err
	})
	if err != nil {
		return nil, err
	}

	if len(n.nets) == 0 {
		return nil, fmt.Errorf("no nets successfully setup")
	}

	if err = n.saveNetInfo(); err != nil {
		return nil, err
	}

	// last net is the default
	n.MetadataIP = n.nets[len(n.nets)-1].ip

	return &n, nil
}

// Teardown cleans up a produced Networking object.
func (n *Networking) Teardown() {
	// Teardown everything in reverse order of setup.
	// This is called during error cases as well, so
	// not everything may be setup.
	// N.B. better to keep going in case of errors
	// to get as much cleaned up as possible.

	if n.contNS == nil || n.hostNS == nil {
		return
	}

	if err := n.EnterHostNS(); err != nil {
		log.Print(err)
		return
	}

	n.teardownNets(n.contNSPath, n.nets)

	if n.contNSPath == "" {
		return
	}

	if err := syscall.Unmount(n.contNSPath, 0); err != nil {
		log.Printf("Error unmounting %q: %v", n.contNSPath, err)
	}
}

// sets up new netns with just lo
func basicNetNS() (hostNS, contNS *os.File, err error) {
	hostNS, contNS, err = newNetNS()
	if err != nil {
		err = fmt.Errorf("failed to create new netns: %v", err)
		return
	}
	// we're in contNS!!

	if err = loUp(); err != nil {
		hostNS.Close()
		contNS.Close()
		return nil, nil, err
	}

	return
}

// EnterHostNS moves into the host's network namespace.
func (n *Networking) EnterHostNS() error {
	return util.SetNS(n.hostNS, syscall.CLONE_NEWNET)
}

// EnterContNS moves into the container's network namespace.
func (n *Networking) EnterContNS() error {
	return util.SetNS(n.contNS, syscall.CLONE_NEWNET)
}

func (e *containerEnv) netDir() string {
	return filepath.Join(e.rktRoot, "net")
}

func (e *containerEnv) setupNets(netns string, nets []Net) ([]activeNet, error) {
	err := os.MkdirAll(e.netDir(), 0755)
	if err != nil {
		return nil, err
	}

	active := []activeNet{}

	for i, nt := range nets {
		log.Printf("Setup: executing net-plugin %v", nt.Type)

		an := activeNet{
			Net:    nt,
			ifName: fmt.Sprintf(ifnamePattern, i),
		}

		if an.Filename, err = copyFileToDir(nt.Filename, e.netDir()); err != nil {
			err = fmt.Errorf("error copying %q to %q: %v", nt.Filename, e.netDir(), err)
			break
		}

		an.ip, err = e.netPluginAdd(&nt, netns, nt.args, an.ifName)
		if err != nil {
			err = fmt.Errorf("error adding network %q: %v", nt.Name, err)
			break
		}

		active = append(active, an)
	}

	if err != nil {
		e.teardownNets(netns, active)
		return nil, err
	}

	return active, nil
}

func (e *containerEnv) teardownNets(netns string, nets []activeNet) {
	for i := len(nets) - 1; i >= 0; i-- {
		nt := nets[i]

		log.Printf("Teardown: executing net-plugin %v", nt.Type)

		err := e.netPluginDel(&nt.Net, netns, nt.args, nt.ifName)
		if err != nil {
			log.Printf("Error deleting %q: %v", nt.Name, err)
		}

		// Delete the conf file to signal that the network was
		// torn down (or at least attempted to)
		if err = os.Remove(nt.Filename); err != nil {
			log.Printf("Error deleting %q: %v", nt.Filename, err)
		}
	}
}

// saveNetInfo writes out the info about active nets
// for "rkt list" and friends to display
func (e *Networking) saveNetInfo() error {
	nis := []netinfo.NetInfo{}
	for _, n := range e.nets {
		ni := netinfo.NetInfo{
			NetName: n.Name,
			IfName:  n.ifName,
			IP:      n.ip.String(),
		}
		nis = append(nis, ni)
	}

	return netinfo.Save(e.rktRoot, nis)
}

func newNetNS() (hostNS, childNS *os.File, err error) {
	defer func() {
		if err != nil {
			if hostNS != nil {
				hostNS.Close()
			}
			if childNS != nil {
				childNS.Close()
			}
		}
	}()

	hostNS, err = os.Open(selfNetNS)
	if err != nil {
		return
	}

	if err = syscall.Unshare(syscall.CLONE_NEWNET); err != nil {
		return
	}

	childNS, err = os.Open(selfNetNS)
	if err != nil {
		util.SetNS(hostNS, syscall.CLONE_NEWNET)
		return
	}

	return
}

// execute f() in tgtNS
func withNetNS(curNS, tgtNS *os.File, f func() error) error {
	if err := util.SetNS(tgtNS, syscall.CLONE_NEWNET); err != nil {
		return err
	}

	if err := f(); err != nil {
		// Attempt to revert the net ns in a known state
		if err := util.SetNS(curNS, syscall.CLONE_NEWNET); err != nil {
			log.Printf("Cannot revert the net namespace: %v", err)
		}
		return err
	}

	return util.SetNS(curNS, syscall.CLONE_NEWNET)
}

func loUp() error {
	lo, err := netlink.LinkByName("lo")
	if err != nil {
		return fmt.Errorf("failed to lookup lo: %v", err)
	}

	if err := netlink.LinkSetUp(lo); err != nil {
		return fmt.Errorf("failed to set lo up: %v", err)
	}

	return nil
}

func bindMountFile(src, dst string) error {
	// mount point has to be an existing file
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	f.Close()

	return syscall.Mount(src, dst, "none", syscall.MS_BIND, "")
}

func copyFileToDir(src, dstdir string) (string, error) {
	dst := filepath.Join(dstdir, filepath.Base(src))

	s, err := os.Open(src)
	if err != nil {
		return "", err
	}
	defer s.Close()

	d, err := os.Create(dst)
	if err != nil {
		return "", err
	}
	defer d.Close()

	_, err = io.Copy(d, s)
	return dst, err
}
