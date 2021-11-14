package headscale

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/fatih/set"
	v1 "github.com/juanfont/headscale/gen/go/headscale/v1"
	"github.com/rs/zerolog/log"
	"google.golang.org/protobuf/types/known/timestamppb"
	"gorm.io/datatypes"
	"gorm.io/gorm"
	"inet.af/netaddr"
	"tailscale.com/tailcfg"
	"tailscale.com/types/wgkey"
)

// Machine is a Headscale client.
type Machine struct {
	ID          uint64 `gorm:"primary_key"`
	MachineKey  string `gorm:"type:varchar(64);unique_index"`
	NodeKey     string
	DiscoKey    string
	IPAddress   string
	Name        string
	NamespaceID uint
	Namespace   Namespace `gorm:"foreignKey:NamespaceID"`

	Registered     bool // temp
	RegisterMethod string
	AuthKeyID      uint
	AuthKey        *PreAuthKey

	LastSeen             *time.Time
	LastSuccessfulUpdate *time.Time
	Expiry               *time.Time
	RequestedExpiry      *time.Time

	HostInfo      datatypes.JSON
	Endpoints     datatypes.JSON
	EnabledRoutes datatypes.JSON

	CreatedAt time.Time
	UpdatedAt time.Time
	DeletedAt *time.Time
}

type (
	Machines  []Machine
	MachinesP []*Machine
)

// For the time being this method is rather naive.
func (m Machine) isAlreadyRegistered() bool {
	return m.Registered
}

// isExpired returns whether the machine registration has expired.
func (m Machine) isExpired() bool {
	return time.Now().UTC().After(*m.Expiry)
}

// If the Machine is expired, updateMachineExpiry updates the Machine Expiry time to the maximum allowed duration,
// or the default duration if no Expiry time was requested by the client. The expiry time here does not (yet) cause
// a client to be disconnected, however they will have to re-auth the machine if they attempt to reconnect after the
// expiry time.
func (h *Headscale) updateMachineExpiry(m *Machine) {
	if m.isExpired() {
		now := time.Now().UTC()
		maxExpiry := now.Add(
			h.cfg.MaxMachineRegistrationDuration,
		) // calculate the maximum expiry
		defaultExpiry := now.Add(
			h.cfg.DefaultMachineRegistrationDuration,
		) // calculate the default expiry

		// clamp the expiry time of the machine registration to the maximum allowed, or use the default if none supplied
		if maxExpiry.Before(*m.RequestedExpiry) {
			log.Debug().
				Msgf("Clamping registration expiry time to maximum: %v (%v)", maxExpiry, h.cfg.MaxMachineRegistrationDuration)
			m.Expiry = &maxExpiry
		} else if m.RequestedExpiry.IsZero() {
			log.Debug().Msgf("Using default machine registration expiry time: %v (%v)", defaultExpiry, h.cfg.DefaultMachineRegistrationDuration)
			m.Expiry = &defaultExpiry
		} else {
			log.Debug().Msgf("Using requested machine registration expiry time: %v", m.RequestedExpiry)
			m.Expiry = m.RequestedExpiry
		}

		h.db.Save(&m)
	}
}

func (h *Headscale) getDirectPeers(m *Machine) (Machines, error) {
	log.Trace().
		Caller().
		Str("machine", m.Name).
		Msg("Finding direct peers")

	machines := Machines{}
	if err := h.db.Preload("Namespace").Where("namespace_id = ? AND machine_key <> ? AND registered",
		m.NamespaceID, m.MachineKey).Find(&machines).Error; err != nil {
		log.Error().Err(err).Msg("Error accessing db")

		return Machines{}, err
	}

	sort.Slice(machines, func(i, j int) bool { return machines[i].ID < machines[j].ID })

	log.Trace().
		Caller().
		Str("machine", m.Name).
		Msgf("Found direct machines: %s", machines.String())

	return machines, nil
}

// getShared fetches machines that are shared to the `Namespace` of the machine we are getting peers for.
func (h *Headscale) getShared(m *Machine) (Machines, error) {
	log.Trace().
		Caller().
		Str("machine", m.Name).
		Msg("Finding shared peers")

	sharedMachines := []SharedMachine{}
	if err := h.db.Preload("Namespace").Preload("Machine").Preload("Machine.Namespace").Where("namespace_id = ?",
		m.NamespaceID).Find(&sharedMachines).Error; err != nil {
		return Machines{}, err
	}

	peers := make(Machines, 0)
	for _, sharedMachine := range sharedMachines {
		peers = append(peers, sharedMachine.Machine)
	}

	sort.Slice(peers, func(i, j int) bool { return peers[i].ID < peers[j].ID })

	log.Trace().
		Caller().
		Str("machine", m.Name).
		Msgf("Found shared peers: %s", peers.String())

	return peers, nil
}

// getSharedTo fetches the machines of the namespaces this machine is shared in.
func (h *Headscale) getSharedTo(m *Machine) (Machines, error) {
	log.Trace().
		Caller().
		Str("machine", m.Name).
		Msg("Finding peers in namespaces this machine is shared with")

	sharedMachines := []SharedMachine{}
	if err := h.db.Preload("Namespace").Preload("Machine").Preload("Machine.Namespace").Where("machine_id = ?",
		m.ID).Find(&sharedMachines).Error; err != nil {
		return Machines{}, err
	}

	peers := make(Machines, 0)
	for _, sharedMachine := range sharedMachines {
		namespaceMachines, err := h.ListMachinesInNamespace(
			sharedMachine.Namespace.Name,
		)
		if err != nil {
			return Machines{}, err
		}
		peers = append(peers, namespaceMachines...)
	}

	sort.Slice(peers, func(i, j int) bool { return peers[i].ID < peers[j].ID })

	log.Trace().
		Caller().
		Str("machine", m.Name).
		Msgf("Found peers we are shared with: %s", peers.String())

	return peers, nil
}

func (h *Headscale) getPeers(m *Machine) (Machines, error) {
	direct, err := h.getDirectPeers(m)
	if err != nil {
		log.Error().
			Caller().
			Err(err).
			Msg("Cannot fetch peers")

		return Machines{}, err
	}

	shared, err := h.getShared(m)
	if err != nil {
		log.Error().
			Caller().
			Err(err).
			Msg("Cannot fetch peers")

		return Machines{}, err
	}

	sharedTo, err := h.getSharedTo(m)
	if err != nil {
		log.Error().
			Caller().
			Err(err).
			Msg("Cannot fetch peers")

		return Machines{}, err
	}

	peers := append(direct, shared...)
	peers = append(peers, sharedTo...)

	sort.Slice(peers, func(i, j int) bool { return peers[i].ID < peers[j].ID })

	log.Trace().
		Caller().
		Str("machine", m.Name).
		Msgf("Found total peers: %s", peers.String())

	return peers, nil
}

func (h *Headscale) ListMachines() ([]Machine, error) {
	machines := []Machine{}
	if err := h.db.Preload("AuthKey").Preload("AuthKey.Namespace").Preload("Namespace").Find(&machines).Error; err != nil {
		return nil, err
	}

	return machines, nil
}

// GetMachine finds a Machine by name and namespace and returns the Machine struct.
func (h *Headscale) GetMachine(namespace string, name string) (*Machine, error) {
	machines, err := h.ListMachinesInNamespace(namespace)
	if err != nil {
		return nil, err
	}

	for _, m := range machines {
		if m.Name == name {
			return &m, nil
		}
	}

	return nil, fmt.Errorf("machine not found")
}

// GetMachineByID finds a Machine by ID and returns the Machine struct.
func (h *Headscale) GetMachineByID(id uint64) (*Machine, error) {
	m := Machine{}
	if result := h.db.Preload("Namespace").Find(&Machine{ID: id}).First(&m); result.Error != nil {
		return nil, result.Error
	}

	return &m, nil
}

// GetMachineByMachineKey finds a Machine by ID and returns the Machine struct.
func (h *Headscale) GetMachineByMachineKey(mKey string) (*Machine, error) {
	m := Machine{}
	if result := h.db.Preload("Namespace").First(&m, "machine_key = ?", mKey); result.Error != nil {
		return nil, result.Error
	}

	return &m, nil
}

// UpdateMachine takes a Machine struct pointer (typically already loaded from database
// and updates it with the latest data from the database.
func (h *Headscale) UpdateMachine(m *Machine) error {
	if result := h.db.Find(m).First(&m); result.Error != nil {
		return result.Error
	}

	return nil
}

// DeleteMachine softs deletes a Machine from the database.
func (h *Headscale) DeleteMachine(m *Machine) error {
	err := h.RemoveSharedMachineFromAllNamespaces(m)
	if err != nil && err != errorMachineNotShared {
		return err
	}

	m.Registered = false
	namespaceID := m.NamespaceID
	h.db.Save(&m) // we mark it as unregistered, just in case
	if err := h.db.Delete(&m).Error; err != nil {
		return err
	}

	return h.RequestMapUpdates(namespaceID)
}

// HardDeleteMachine hard deletes a Machine from the database.
func (h *Headscale) HardDeleteMachine(m *Machine) error {
	err := h.RemoveSharedMachineFromAllNamespaces(m)
	if err != nil && err != errorMachineNotShared {
		return err
	}

	namespaceID := m.NamespaceID
	if err := h.db.Unscoped().Delete(&m).Error; err != nil {
		return err
	}

	return h.RequestMapUpdates(namespaceID)
}

// GetHostInfo returns a Hostinfo struct for the machine.
func (m *Machine) GetHostInfo() (*tailcfg.Hostinfo, error) {
	hostinfo := tailcfg.Hostinfo{}
	if len(m.HostInfo) != 0 {
		hi, err := m.HostInfo.MarshalJSON()
		if err != nil {
			return nil, err
		}
		err = json.Unmarshal(hi, &hostinfo)
		if err != nil {
			return nil, err
		}
	}

	return &hostinfo, nil
}

func (h *Headscale) isOutdated(m *Machine) bool {
	if err := h.UpdateMachine(m); err != nil {
		// It does not seem meaningful to propagate this error as the end result
		// will have to be that the machine has to be considered outdated.
		return true
	}

	sharedMachines, _ := h.getShared(m)

	namespaceSet := set.New(set.ThreadSafe)
	namespaceSet.Add(m.Namespace.Name)

	// Check if any of our shared namespaces has updates that we have
	// not propagated.
	for _, sharedMachine := range sharedMachines {
		namespaceSet.Add(sharedMachine.Namespace.Name)
	}

	namespaces := make([]string, namespaceSet.Size())
	for index, namespace := range namespaceSet.List() {
		namespaces[index] = namespace.(string)
	}

	lastChange := h.getLastStateChange(namespaces...)
	log.Trace().
		Caller().
		Str("machine", m.Name).
		Time("last_successful_update", *m.LastSuccessfulUpdate).
		Time("last_state_change", lastChange).
		Msgf("Checking if %s is missing updates", m.Name)

	return m.LastSuccessfulUpdate.Before(lastChange)
}

func (m Machine) String() string {
	return m.Name
}

func (ms Machines) String() string {
	temp := make([]string, len(ms))

	for index, machine := range ms {
		temp[index] = machine.Name
	}

	return fmt.Sprintf("[ %s ](%d)", strings.Join(temp, ", "), len(temp))
}

// TODO(kradalby): Remove when we have generics...
func (ms MachinesP) String() string {
	temp := make([]string, len(ms))

	for index, machine := range ms {
		temp[index] = machine.Name
	}

	return fmt.Sprintf("[ %s ](%d)", strings.Join(temp, ", "), len(temp))
}

func (ms Machines) toNodes(
	baseDomain string,
	dnsConfig *tailcfg.DNSConfig,
	includeRoutes bool,
) ([]*tailcfg.Node, error) {
	nodes := make([]*tailcfg.Node, len(ms))

	for index, machine := range ms {
		node, err := machine.toNode(baseDomain, dnsConfig, includeRoutes)
		if err != nil {
			return nil, err
		}

		nodes[index] = node
	}

	return nodes, nil
}

// toNode converts a Machine into a Tailscale Node. includeRoutes is false for shared nodes
// as per the expected behaviour in the official SaaS.
func (m Machine) toNode(
	baseDomain string,
	dnsConfig *tailcfg.DNSConfig,
	includeRoutes bool,
) (*tailcfg.Node, error) {
	nKey, err := wgkey.ParseHex(m.NodeKey)
	if err != nil {
		return nil, err
	}
	mKey, err := wgkey.ParseHex(m.MachineKey)
	if err != nil {
		return nil, err
	}

	var discoKey tailcfg.DiscoKey
	if m.DiscoKey != "" {
		dKey, err := wgkey.ParseHex(m.DiscoKey)
		if err != nil {
			return nil, err
		}
		discoKey = tailcfg.DiscoKey(dKey)
	} else {
		discoKey = tailcfg.DiscoKey{}
	}

	addrs := []netaddr.IPPrefix{}
	ip, err := netaddr.ParseIPPrefix(fmt.Sprintf("%s/32", m.IPAddress))
	if err != nil {
		log.Trace().
			Caller().
			Str("ip", m.IPAddress).
			Msgf("Failed to parse IP Prefix from IP: %s", m.IPAddress)

		return nil, err
	}
	addrs = append(addrs, ip) // missing the ipv6 ?

	allowedIPs := []netaddr.IPPrefix{}
	allowedIPs = append(
		allowedIPs,
		ip,
	) // we append the node own IP, as it is required by the clients

	if includeRoutes {
		routesStr := []string{}
		if len(m.EnabledRoutes) != 0 {
			allwIps, err := m.EnabledRoutes.MarshalJSON()
			if err != nil {
				return nil, err
			}
			err = json.Unmarshal(allwIps, &routesStr)
			if err != nil {
				return nil, err
			}
		}

		for _, routeStr := range routesStr {
			ip, err := netaddr.ParseIPPrefix(routeStr)
			if err != nil {
				return nil, err
			}
			allowedIPs = append(allowedIPs, ip)
		}
	}

	endpoints := []string{}
	if len(m.Endpoints) != 0 {
		be, err := m.Endpoints.MarshalJSON()
		if err != nil {
			return nil, err
		}
		err = json.Unmarshal(be, &endpoints)
		if err != nil {
			return nil, err
		}
	}

	hostinfo := tailcfg.Hostinfo{}
	if len(m.HostInfo) != 0 {
		hi, err := m.HostInfo.MarshalJSON()
		if err != nil {
			return nil, err
		}
		err = json.Unmarshal(hi, &hostinfo)
		if err != nil {
			return nil, err
		}
	}

	var derp string
	if hostinfo.NetInfo != nil {
		derp = fmt.Sprintf("127.3.3.40:%d", hostinfo.NetInfo.PreferredDERP)
	} else {
		derp = "127.3.3.40:0" // Zero means disconnected or unknown.
	}

	var keyExpiry time.Time
	if m.Expiry != nil {
		keyExpiry = *m.Expiry
	} else {
		keyExpiry = time.Time{}
	}

	var hostname string
	if dnsConfig != nil && dnsConfig.Proxied { // MagicDNS
		hostname = fmt.Sprintf("%s.%s.%s", m.Name, m.Namespace.Name, baseDomain)
	} else {
		hostname = m.Name
	}

	n := tailcfg.Node{
		ID: tailcfg.NodeID(m.ID), // this is the actual ID
		StableID: tailcfg.StableNodeID(
			strconv.FormatUint(m.ID, BASE_10),
		), // in headscale, unlike tailcontrol server, IDs are permanent
		Name:       hostname,
		User:       tailcfg.UserID(m.NamespaceID),
		Key:        tailcfg.NodeKey(nKey),
		KeyExpiry:  keyExpiry,
		Machine:    tailcfg.MachineKey(mKey),
		DiscoKey:   discoKey,
		Addresses:  addrs,
		AllowedIPs: allowedIPs,
		Endpoints:  endpoints,
		DERP:       derp,

		Hostinfo: hostinfo,
		Created:  m.CreatedAt,
		LastSeen: m.LastSeen,

		KeepAlive:         true,
		MachineAuthorized: m.Registered,
		Capabilities:      []string{tailcfg.CapabilityFileSharing},
	}

	return &n, nil
}

func (m *Machine) toProto() *v1.Machine {
	machine := &v1.Machine{
		Id:         m.ID,
		MachineKey: m.MachineKey,

		NodeKey:   m.NodeKey,
		DiscoKey:  m.DiscoKey,
		IpAddress: m.IPAddress,
		Name:      m.Name,
		Namespace: m.Namespace.toProto(),

		Registered: m.Registered,

		// TODO(kradalby): Implement register method enum converter
		// RegisterMethod: ,

		CreatedAt: timestamppb.New(m.CreatedAt),
	}

	if m.AuthKey != nil {
		machine.PreAuthKey = m.AuthKey.toProto()
	}

	if m.LastSeen != nil {
		machine.LastSeen = timestamppb.New(*m.LastSeen)
	}

	if m.LastSuccessfulUpdate != nil {
		machine.LastSuccessfulUpdate = timestamppb.New(*m.LastSuccessfulUpdate)
	}

	if m.Expiry != nil {
		machine.Expiry = timestamppb.New(*m.Expiry)
	}

	return machine
}

// RegisterMachine is executed from the CLI to register a new Machine using its MachineKey.
func (h *Headscale) RegisterMachine(key string, namespace string) (*Machine, error) {
	ns, err := h.GetNamespace(namespace)
	if err != nil {
		return nil, err
	}
	mKey, err := wgkey.ParseHex(key)
	if err != nil {
		return nil, err
	}

	m := Machine{}
	if result := h.db.First(&m, "machine_key = ?", mKey.HexString()); errors.Is(
		result.Error,
		gorm.ErrRecordNotFound,
	) {
		return nil, errors.New("Machine not found")
	}

	log.Trace().
		Caller().
		Str("machine", m.Name).
		Msg("Attempting to register machine")

	if m.isAlreadyRegistered() {
		err := errors.New("Machine already registered")
		log.Error().
			Caller().
			Err(err).
			Str("machine", m.Name).
			Msg("Attempting to register machine")

		return nil, err
	}

	ip, err := h.getAvailableIP()
	if err != nil {
		log.Error().
			Caller().
			Err(err).
			Str("machine", m.Name).
			Msg("Could not find IP for the new machine")

		return nil, err
	}

	log.Trace().
		Caller().
		Str("machine", m.Name).
		Str("ip", ip.String()).
		Msg("Found IP for host")

	m.IPAddress = ip.String()
	m.NamespaceID = ns.ID
	m.Registered = true
	m.RegisterMethod = "cli"
	h.db.Save(&m)

	log.Trace().
		Caller().
		Str("machine", m.Name).
		Str("ip", ip.String()).
		Msg("Machine registered with the database")

	return &m, nil
}

func (m *Machine) GetAdvertisedRoutes() ([]netaddr.IPPrefix, error) {
	hostInfo, err := m.GetHostInfo()
	if err != nil {
		return nil, err
	}

	return hostInfo.RoutableIPs, nil
}

func (m *Machine) GetEnabledRoutes() ([]netaddr.IPPrefix, error) {
	data, err := m.EnabledRoutes.MarshalJSON()
	if err != nil {
		return nil, err
	}

	routesStr := []string{}
	err = json.Unmarshal(data, &routesStr)
	if err != nil {
		return nil, err
	}

	routes := make([]netaddr.IPPrefix, len(routesStr))
	for index, routeStr := range routesStr {
		route, err := netaddr.ParseIPPrefix(routeStr)
		if err != nil {
			return nil, err
		}
		routes[index] = route
	}

	return routes, nil
}

func (m *Machine) IsRoutesEnabled(routeStr string) bool {
	route, err := netaddr.ParseIPPrefix(routeStr)
	if err != nil {
		return false
	}

	enabledRoutes, err := m.GetEnabledRoutes()
	if err != nil {
		return false
	}

	for _, enabledRoute := range enabledRoutes {
		if route == enabledRoute {
			return true
		}
	}

	return false
}

// EnableNodeRoute enables new routes based on a list of new routes. It will _replace_ the
// previous list of routes.
func (h *Headscale) EnableRoutes(m *Machine, routeStrs ...string) error {
	newRoutes := make([]netaddr.IPPrefix, len(routeStrs))
	for index, routeStr := range routeStrs {
		route, err := netaddr.ParseIPPrefix(routeStr)
		if err != nil {
			return err
		}

		newRoutes[index] = route
	}

	availableRoutes, err := m.GetAdvertisedRoutes()
	if err != nil {
		return err
	}

	for _, newRoute := range newRoutes {
		if !containsIpPrefix(availableRoutes, newRoute) {
			return fmt.Errorf(
				"route (%s) is not available on node %s",
				m.Name,
				newRoute,
			)
		}
	}

	routes, err := json.Marshal(newRoutes)
	if err != nil {
		return err
	}

	m.EnabledRoutes = datatypes.JSON(routes)
	h.db.Save(&m)

	err = h.RequestMapUpdates(m.NamespaceID)
	if err != nil {
		return err
	}

	return nil
}

func (m *Machine) RoutesToProto() (*v1.Routes, error) {
	availableRoutes, err := m.GetAdvertisedRoutes()
	if err != nil {
		return nil, err
	}

	enabledRoutes, err := m.GetEnabledRoutes()
	if err != nil {
		return nil, err
	}

	return &v1.Routes{
		AdvertisedRoutes: ipPrefixToString(availableRoutes),
		EnabledRoutes:    ipPrefixToString(enabledRoutes),
	}, nil
}
