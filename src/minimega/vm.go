// Copyright (2012) Sandia Corporation.
// Under the terms of Contract DE-AC04-94AL85000 with Sandia Corporation,
// the U.S. Government retains certain rights in this software.

package main

import (
	"errors"
	"fmt"
	"minicli"
	log "minilog"
	"sort"
	"strconv"
	"strings"
	"sync"
)

var (
	vmConfig *VMConfig // current vm config, updated by CLI

	killAck  chan int
	vmIdChan chan int
	vmLock   sync.Mutex
)

const (
	VM_MEMORY_DEFAULT     = "2048"
	VM_NET_DRIVER_DEFAULT = "e1000"
	QMP_CONNECT_RETRY     = 50
	QMP_CONNECT_DELAY     = 100
)

type VM interface {
	Config() *VMConfig

	ID() int
	Name() string
	State() VMState

	Launch(string, chan int) error
	// TODO: Make kill have ack channel?
	Kill() error
	Start() error
	Stop() error

	String() string
	Info(masks []string) ([]string, error)

	Tags() []string
	Tag(string) string
	SetTag(string, string)
	ClearTag(string)
	ClearTags()
}

type VMConfig struct {
	Vcpus  string // number of virtual cpus
	Memory string // memory for the vm, in megabytes

	Networks []NetConfig // ordered list of networks
}

type NetConfig struct {
	VLAN   int
	Bridge string
	Tap    string
	MAC    string
	Driver string
}

type vmBase struct {
	lock sync.Mutex

	id   int
	name string

	state VMState

	tags map[string]string
}

// Valid names for output masks for vm info, in preferred output order
var vmMasks = []string{
	"id", "name", "state", "memory", "vcpus", "migrate", "disk", "snapshot",
	"initrd", "kernel", "cdrom", "append", "bridge", "tap", "bandwidth", "mac",
	"ip", "ip6", "vlan", "uuid", "cc_active", "tags",
}

func NewVM() *vmBase {
	vm := new(vmBase)

	vm.state = VM_BUILDING
	vm.tags = make(map[string]string)

	return vm
}

func (old *VMConfig) Copy() *VMConfig {
	res := new(VMConfig)

	// Copy all fields
	*res = *old

	// Make deep copy of slices
	res.Networks = make([]NetConfig, len(old.Networks))
	copy(res.Networks, old.Networks)

	return res
}

func (vm *VMConfig) NetworkString() string {
	parts := []string{}
	for _, net := range vm.Networks {
		parts = append(parts, net.String())
	}

	return fmt.Sprintf("[%s]", strings.Join(parts, " "))
}

// TODO: Handle if there are spaces or commas in the tap/bridge names
func (net NetConfig) String() (s string) {
	parts := []string{}
	if net.Bridge != "" {
		parts = append(parts, net.Bridge)
	}

	parts = append(parts, strconv.Itoa(net.VLAN))

	if net.MAC != "" {
		parts = append(parts, net.MAC)
	}

	return strings.Join(parts, ",")
}

func (vm *vmBase) ID() int {
	return vm.id
}

func (vm *vmBase) Name() string {
	return vm.name
}

func (vm *vmBase) State() VMState {
	vm.lock.Lock()
	defer vm.lock.Unlock()

	return vm.state
}

func (vm *vmBase) Tags() (tags []string) {
	for _, v := range vm.tags {
		tags = append(tags, v)
	}
	return
}

func (vm *vmBase) Tag(k string) string {
	return vm.tags[k]
}

func (vm *vmBase) SetTag(k, v string) {
	vm.tags[k] = v
}

func (vm *vmBase) ClearTags() {
	vm.tags = make(map[string]string)
}

func (vm *vmBase) ClearTag(k string) {
	delete(vm.tags, k)
}

func init() {
	killAck = make(chan int)

	vmConfig = &VMConfig{}

	vmIdChan = makeIDChan()

	// Reset everything to default
	for _, fns := range vmConfigFns {
		fns.Clear(vmConfig)
	}
}

func vmNotFound(idOrName string) error {
	return fmt.Errorf("vm not found: %v", idOrName)
}

// satisfy the sort interface for vmInfo
func SortBy(by string, vms []*vmKVM) {
	v := &vmSorter{
		vms: vms,
		by:  by,
	}
	sort.Sort(v)
}

type vmSorter struct {
	vms []*vmKVM
	by  string
}

func (vms *vmSorter) Len() int {
	return len(vms.vms)
}

func (vms *vmSorter) Swap(i, j int) {
	vms.vms[i], vms.vms[j] = vms.vms[j], vms.vms[i]
}

func (vms *vmSorter) Less(i, j int) bool {
	switch vms.by {
	case "id":
		return vms.vms[i].id < vms.vms[j].id
	case "host":
		return true
	case "name":
		return vms.vms[i].name < vms.vms[j].name
	case "state":
		return vms.vms[i].State() < vms.vms[j].State()
	case "memory":
		return vms.vms[i].Memory < vms.vms[j].Memory
	case "vcpus":
		return vms.vms[i].Vcpus < vms.vms[j].Vcpus
	case "migrate":
		return vms.vms[i].MigratePath < vms.vms[j].MigratePath
	case "disk":
		return len(vms.vms[i].DiskPaths) < len(vms.vms[j].DiskPaths)
	case "initrd":
		return vms.vms[i].InitrdPath < vms.vms[j].InitrdPath
	case "kernel":
		return vms.vms[i].KernelPath < vms.vms[j].KernelPath
	case "cdrom":
		return vms.vms[i].CdromPath < vms.vms[j].CdromPath
	case "append":
		return vms.vms[i].Append < vms.vms[j].Append
	case "bridge", "tap", "mac", "ip", "ip6", "vlan":
		return true
	case "uuid":
		return vms.vms[i].UUID < vms.vms[j].UUID
	case "cc_active":
		return true
	case "tags":
		return true
	default:
		log.Error("invalid sort parameter %v", vms.by)
		return false
	}
}

func vmGetAllSerialPorts() []string {
	vmLock.Lock()
	defer vmLock.Unlock()

	mask := VM_BUILDING | VM_RUNNING | VM_PAUSED

	var ret []string
	for _, vm := range vms {
		// TODO: non-kvm VMs?
		if vm, ok := vm.(*vmKVM); ok && vm.State()&mask != 0 {
			ret = append(ret, vm.instancePath+"serial")
		}
	}
	return ret
}

// processVMNet processes the input specifying the bridge, vlan, and mac for
// one interface to a VM and updates the vm config accordingly. This takes a
// bit of parsing, because the entry can be in a few forms:
// 	vlan
//
//	vlan,mac
//	bridge,vlan
//	vlan,driver
//
//	bridge,vlan,mac
//	vlan,mac,driver
//	bridge,vlan,driver
//
//	bridge,vlan,mac,driver
// If there are 2 or 3 fields, just the last field for the presence of a mac
func processVMNet(spec string) (res NetConfig, err error) {
	// example: my_bridge,100,00:00:00:00:00:00
	f := strings.Split(spec, ",")

	var b string
	var v string
	var m string
	var d string
	switch len(f) {
	case 1:
		v = f[0]
	case 2:
		if isMac(f[1]) {
			// vlan, mac
			v = f[0]
			m = f[1]
		} else if _, err := strconv.Atoi(f[0]); err == nil {
			// vlan, driver
			v = f[0]
			d = f[1]
		} else {
			// bridge, vlan
			b = f[0]
			v = f[1]
		}
	case 3:
		if isMac(f[2]) {
			// bridge, vlan, mac
			b = f[0]
			v = f[1]
			m = f[2]
		} else if isMac(f[1]) {
			// vlan, mac, driver
			v = f[0]
			m = f[1]
			d = f[2]
		} else {
			// bridge, vlan, driver
			b = f[0]
			v = f[1]
			d = f[2]
		}
	case 4:
		b = f[0]
		v = f[1]
		m = f[2]
		d = f[3]
	default:
		err = errors.New("malformed netspec")
		return
	}

	log.Debug("vm_net got b=%v, v=%v, m=%v, d=%v", b, v, m, d)

	// VLAN ID, with optional bridge
	vlan, err := strconv.Atoi(v) // the vlan id
	if err != nil {
		err = errors.New("malformed netspec, vlan must be an integer")
		return
	}

	if m != "" && !isMac(m) {
		err = errors.New("malformed netspec, invalid mac address: " + m)
		return
	}

	var currBridge *bridge
	currBridge, err = getBridge(b)
	if err != nil {
		return
	}

	err = currBridge.LanCreate(vlan)
	if err != nil {
		return
	}

	if b == "" {
		b = DEFAULT_BRIDGE
	}
	if d == "" {
		d = VM_NET_DRIVER_DEFAULT
	}

	res = NetConfig{
		VLAN:   vlan,
		Bridge: b,
		MAC:    strings.ToLower(m),
		Driver: d,
	}

	return
}

// Get the VM info from all hosts optionally applying column/row filters.
// Returns a map with keys for the hostnames and values as the tabular data
// from the host.
func globalVmInfo(masks []string, filters []string) (map[string]VMs, map[string]minicli.Responses) {
	cmdStr := "vm info"
	for _, v := range filters {
		cmdStr = fmt.Sprintf(".filter %s %s", v, cmdStr)
	}
	if len(masks) > 0 {
		cmdStr = fmt.Sprintf(".columns %s %s", strings.Join(masks, ","), cmdStr)
	}

	res := map[string]VMs{}
	res2 := map[string]minicli.Responses{}

	for resps := range runCommandGlobally(minicli.MustCompile(cmdStr), false) {
		for _, resp := range resps {
			if resp.Error != "" {
				log.Errorln(resp.Error)
				continue
			}

			switch data := resp.Data.(type) {
			case VMs:
				res[resp.Host] = data
			default:
				log.Error("unknown data field in vm info")
			}

			res2[resp.Host] = append(res2[resp.Host], resp)
		}
	}

	return res, res2
}

// mustFindMask returns the index of the specified mask in vmMasks. If the
// specified mask is not found, log.Fatal is called.
func mustFindMask(mask string) int {
	for i, v := range vmMasks {
		if v == mask {
			return i
		}
	}

	log.Fatal("missing `%s` in vmMasks", mask)
	return -1
}
