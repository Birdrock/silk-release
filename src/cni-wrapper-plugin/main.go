package main

import (
	"cni-wrapper-plugin/adapter"
	"cni-wrapper-plugin/interfacelookup"
	"cni-wrapper-plugin/legacynet"
	"cni-wrapper-plugin/lib"
	"encoding/json"
	"fmt"
	"lib/datastore"
	"lib/rules"
	"lib/serial"
	"net"
	"os"
	"sync"

	"io/ioutil"
	"net/http"

	"code.cloudfoundry.org/filelock"
	"github.com/containernetworking/cni/pkg/skel"
	"github.com/containernetworking/cni/pkg/types/current"
	"github.com/containernetworking/cni/pkg/version"
	"github.com/coreos/go-iptables/iptables"
)

func cmdAdd(args *skel.CmdArgs) error {
	cfg, err := lib.LoadWrapperConfig(args.StdinData)
	if err != nil {
		return err
	}

	pluginController, err := newPluginController(cfg.IPTablesLockFile)
	if err != nil {
		return err
	}

	result, err := pluginController.DelegateAdd(cfg.Delegate)
	if err != nil {
		return fmt.Errorf("delegate call: %s", err)
	}

	result030, err := current.NewResultFromResult(result)
	if err != nil {
		return fmt.Errorf("converting result from delegate plugin: %s", err) // not tested
	}

	containerIP := result030.IPs[0].Address.IP

	// Add container metadata info
	store := &datastore.Store{
		Serializer: &serial.Serial{},
		Locker: &filelock.Locker{
			FileLocker: filelock.NewLocker(cfg.Datastore + "_lock"),
			Mutex:      new(sync.Mutex),
		},
		DataFilePath:    cfg.Datastore,
		VersionFilePath: cfg.Datastore + "_version",
		LockedFilePath:  cfg.Datastore + "_lock",
		FileOwner:       "vcap",
		FileGroup:       "vcap",
		CacheMutex:      new(sync.RWMutex),
	}

	var cniAddData struct {
		Metadata map[string]interface{}
	}
	if err := json.Unmarshal(args.StdinData, &cniAddData); err != nil {
		return err // not tested, this should be impossible
	}

	if err := store.Add(args.ContainerID, containerIP.String(), cniAddData.Metadata); err != nil {
		storeErr := fmt.Errorf("store add: %s", err)
		fmt.Fprintf(os.Stderr, "%s", storeErr)
		fmt.Fprint(os.Stderr, "cleaning up from error")
		err = pluginController.DelIPMasq(containerIP.String(), cfg.NoMasqueradeCIDRRange, cfg.VTEPName)
		if err != nil {
			fmt.Fprintf(os.Stderr, "during cleanup: removing IP masq: %s", err)
		}

		return storeErr
	}

	resp, err := http.DefaultClient.Get(fmt.Sprintf("http://%s/force-policy-poll-cycle", cfg.PolicyAgentForcePollAddress))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := ioutil.ReadAll(resp.Body)
		resp.Body.Close()
		return fmt.Errorf("vpa response code: %v with message: %s", resp.StatusCode, body)
	}

	localDNSServers, err := getLocalDNSServers(cfg.DNSServers)
	if err != nil {
		return err
	}

	interfaceNameLookup := interfacelookup.InterfaceNameLookup{
		NetlinkAdapter: &adapter.NetlinkAdapter{},
	}

	var interfaceNames []string
	if len(cfg.TemporaryUnderlayInterfaceNames) > 0 {
		interfaceNames = cfg.TemporaryUnderlayInterfaceNames
	} else {
		interfaceNames, err = interfaceNameLookup.GetNamesFromIPs(cfg.UnderlayIPs)
		if err != nil {
			return fmt.Errorf("looking up interface names: %s", err) // not tested
		}
	}

	if args.ContainerID == "" {
		return fmt.Errorf("invalid Container ID")
	}

	netOutProvider := legacynet.NetOut{
		ChainNamer: &legacynet.ChainNamer{
			MaxLength: 28,
		},
		IPTables:              pluginController.IPTables,
		Converter:             &legacynet.NetOutRuleConverter{Logger: os.Stderr},
		ASGLogging:            cfg.IPTablesASGLogging,
		C2CLogging:            cfg.IPTablesC2CLogging,
		DeniedLogsPerSec:      cfg.IPTablesDeniedLogsPerSec,
		AcceptedUDPLogsPerSec: cfg.IPTablesAcceptedUDPLogsPerSec,
		IngressTag:            cfg.IngressTag,
		VTEPName:              cfg.VTEPName,
		HostInterfaceNames:    interfaceNames,
		ContainerHandle:       args.ContainerID,
		ContainerIP:           containerIP.String(),
		HostTCPServices:       cfg.HostTCPServices,
		DNSServers:            localDNSServers,
	}
	if err := netOutProvider.Initialize(); err != nil {
		return fmt.Errorf("initialize net out: %s", err)
	}

	netinProvider := legacynet.NetIn{
		ChainNamer: &legacynet.ChainNamer{
			MaxLength: 28,
		},
		IPTables:           pluginController.IPTables,
		IngressTag:         cfg.IngressTag,
		HostInterfaceNames: interfaceNames,
	}
	err = netinProvider.Initialize(args.ContainerID)

	portMappings := cfg.RuntimeConfig.PortMappings
	for _, netIn := range portMappings {
		if netIn.HostPort <= 0 {
			return fmt.Errorf("cannot allocate port %d", netIn.HostPort)
		}
		if err := netinProvider.AddRule(args.ContainerID, int(netIn.HostPort), int(netIn.ContainerPort), cfg.InstanceAddress, containerIP.String()); err != nil {
			return fmt.Errorf("adding netin rule: %s", err)
		}
	}

	netOutRules := cfg.RuntimeConfig.NetOutRules
	if err := netOutProvider.BulkInsertRules(netOutRules); err != nil {
		return fmt.Errorf("bulk insert: %s", err) // not tested
	}

	err = pluginController.AddIPMasq(containerIP.String(), cfg.NoMasqueradeCIDRRange, cfg.VTEPName)
	if err != nil {
		return fmt.Errorf("error setting up default ip masq rule: %s", err)
	}

	result030.DNS.Nameservers = cfg.DNSServers
	return result030.Print()
}

func getLocalDNSServers(allDNSServers []string) ([]string, error) {
	var localDNSServers []string
	for _, entry := range allDNSServers {
		dnsIP := net.ParseIP(entry)
		if dnsIP == nil {
			return nil, fmt.Errorf(`invalid DNS server "%s", must be valid IP address`, entry)
		} else if dnsIP.IsLinkLocalUnicast() {
			localDNSServers = append(localDNSServers, entry)
		}
	}
	return localDNSServers, nil
}

func cmdDel(args *skel.CmdArgs) error {
	n, err := lib.LoadWrapperConfig(args.StdinData)
	if err != nil {
		return err
	}

	store := &datastore.Store{
		Serializer: &serial.Serial{},
		Locker: &filelock.Locker{
			FileLocker: filelock.NewLocker(n.Datastore + "_lock"),
			Mutex:      new(sync.Mutex),
		},
		DataFilePath:    n.Datastore,
		VersionFilePath: n.Datastore + "_version",
		CacheMutex:      new(sync.RWMutex),
	}

	container, err := store.Delete(args.ContainerID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "store delete: %s", err)
	}

	pluginController, err := newPluginController(n.IPTablesLockFile)
	if err != nil {
		return err
	}

	if err := pluginController.DelegateDel(n.Delegate); err != nil {
		fmt.Fprintf(os.Stderr, "delegate delete: %s", err)
	}

	netInProvider := legacynet.NetIn{
		ChainNamer: &legacynet.ChainNamer{
			MaxLength: 28,
		},
		IPTables:   pluginController.IPTables,
		IngressTag: n.IngressTag,
	}

	if err = netInProvider.Cleanup(args.ContainerID); err != nil {
		fmt.Fprintf(os.Stderr, "net in cleanup: %s", err)
	}

	interfaceNameLookup := interfacelookup.InterfaceNameLookup{
		NetlinkAdapter: &adapter.NetlinkAdapter{},
	}

	var interfaceNames []string
	if len(n.TemporaryUnderlayInterfaceNames) > 0 {
		interfaceNames = n.TemporaryUnderlayInterfaceNames
	} else {
		interfaceNames, err = interfaceNameLookup.GetNamesFromIPs(n.UnderlayIPs)
		if err != nil {
			return fmt.Errorf("looking up interface names: %s", err) // not tested
		}
	}

	netOutProvider := legacynet.NetOut{
		ChainNamer: &legacynet.ChainNamer{
			MaxLength: 28,
		},
		IPTables:           pluginController.IPTables,
		Converter:          &legacynet.NetOutRuleConverter{Logger: os.Stderr},
		ContainerHandle:    args.ContainerID,
		ContainerIP:        container.IP,
		HostInterfaceNames: interfaceNames,
	}

	if err = netOutProvider.Cleanup(); err != nil {
		fmt.Fprintf(os.Stderr, "net out cleanup: %s", err)
	}

	err = pluginController.DelIPMasq(container.IP, n.NoMasqueradeCIDRRange, n.VTEPName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "removing IP masq: %s", err)
	}

	return nil
}

func newPluginController(iptablesLockFile string) (*lib.PluginController, error) {
	ipt, err := iptables.New()
	if err != nil {
		return nil, err
	}

	iptLocker := &filelock.Locker{
		FileLocker: filelock.NewLocker(iptablesLockFile),
		Mutex:      &sync.Mutex{},
	}
	restorer := &rules.Restorer{}
	lockedIPTables := &rules.LockedIPTables{
		IPTables: ipt,
		Locker:   iptLocker,
		Restorer: restorer,
	}

	pluginController := &lib.PluginController{
		Delegator: lib.NewDelegator(),
		IPTables:  lockedIPTables,
	}
	return pluginController, nil
}

func main() {
	supportedVersions := []string{"0.3.1"}

	skel.PluginMain(cmdAdd, cmdDel, version.PluginSupports(supportedVersions...))
}
