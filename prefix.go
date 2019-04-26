package ipam

import (
	"fmt"
	"net"
	"sync"
)

// Prefix is a expression of a ip with length and forms a classless network.
type Prefix struct {
	sync.Mutex
	Cidr                   string            // The Cidr of this prefix
	AvailableChildPrefixes map[string]Prefix // available child prefixes of this prefix
	AcquiredChildPrefixes  map[string]Prefix // acquired child prefixes of this prefix
	ChildPrefixLength      int               // the length of the child prefixes
	IPs                    map[string]IP     // The ips contained in this prefix
}

// NewPrefix create a new Prefix from a string notation.
func (i *Ipamer) NewPrefix(cidr string) (*Prefix, error) {
	p, err := i.newPrefix(cidr)
	if err != nil {
		return nil, fmt.Errorf("NewPrefix:%s %v", cidr, err)
	}
	newPrefix, err := i.storage.CreatePrefix(p)
	if err != nil {
		return nil, fmt.Errorf("newPrefix:%s %v", cidr, err)
	}

	return newPrefix, nil
}

// AcquireChildPrefix will return a Prefix with a smaller length from the given Prefix.
// FIXME allow variable child prefix length
func (i *Ipamer) AcquireChildPrefix(prefix *Prefix, length int) (*Prefix, error) {
	prefix.Lock()
	defer prefix.Unlock()
	ipnet, err := prefix.IPNet()
	if err != nil {
		return nil, err
	}
	ones, size := ipnet.Mask.Size()
	if ones >= length {
		return nil, fmt.Errorf("given length:%d is smaller or equal of prefix length:%d", length, ones)
	}

	// If this is the first call, create a pool of available child prefixes with given length upfront
	if prefix.ChildPrefixLength == 0 {
		// power of 2 :-(
		ip := ipnet.IP
		subnetCount := 1 << (uint(length - ones))
		for s := 0; s < subnetCount; s++ {
			ipPart, err := insertNumIntoIP(ip, s, length)
			if err != nil {
				return nil, err
			}
			newIP := &net.IPNet{
				IP:   *ipPart,
				Mask: net.CIDRMask(length, size),
			}
			newCidr := newIP.String()
			child, err := i.newPrefix(newCidr)
			if err != nil {
				return nil, err
			}
			prefix.AvailableChildPrefixes[child.Cidr] = *child

		}
		prefix.ChildPrefixLength = length
	}
	if prefix.ChildPrefixLength != length {
		return nil, fmt.Errorf("given length:%d is not equal to existing child prefix length:%d", length, prefix.ChildPrefixLength)
	}

	var child *Prefix
	for _, v := range prefix.AvailableChildPrefixes {
		child = &v
		break
	}
	if child == nil {
		return nil, fmt.Errorf("no more child prefixes contained in prefix pool")
	}

	delete(prefix.AvailableChildPrefixes, child.Cidr)
	prefix.AcquiredChildPrefixes[child.Cidr] = *child

	i.storage.UpdatePrefix(prefix)
	child, err = i.NewPrefix(child.Cidr)
	if err != nil {
		return nil, fmt.Errorf("unable to persist created child:%v", err)
	}
	return child, nil
}

// ReleaseChildPrefix will mark this child Prefix as available again.
func (i *Ipamer) ReleaseChildPrefix(child *Prefix) error {
	parent := i.getParentPrefix(child)

	if parent == nil {
		return fmt.Errorf("given prefix is no child prefix")
	}
	parent.Lock()
	defer parent.Unlock()

	delete(parent.AcquiredChildPrefixes, child.Cidr)
	parent.AvailableChildPrefixes[child.Cidr] = *child
	_, err := i.storage.UpdatePrefix(parent)
	if err != nil {
		parent.AcquiredChildPrefixes[child.Cidr] = *child
		return fmt.Errorf("unable to release prefix %v:%v", child, err)
	}
	return nil
}

// PrefixFrom will return a known Prefix
func (i *Ipamer) PrefixFrom(cidr string) *Prefix {
	prefixes, err := i.storage.ReadAllPrefixes()
	if err != nil {
		return nil
	}
	_, targetIpnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return nil
	}
	for _, p := range prefixes {
		ipnet, err := p.IPNet()
		if err != nil {
			return nil
		}
		if ipnet.IP.String() == targetIpnet.IP.String() && ipnet.Mask.String() == targetIpnet.Mask.String() {
			return p
		}
	}
	return nil
}

func (i *Ipamer) getPrefixOfIP(ip *IP) *Prefix {
	prefixes, err := i.storage.ReadAllPrefixes()
	if err != nil {
		return nil
	}
	for _, p := range prefixes {
		ipnet, err := p.IPNet()
		if err != nil {
			return nil
		}
		if ipnet.Contains(ip.IP) && ipnet.Mask.String() == ip.IPNet.Mask.String() {
			return p
		}
	}
	return nil
}

func (i *Ipamer) getParentPrefix(prefix *Prefix) *Prefix {
	prefixes, err := i.storage.ReadAllPrefixes()
	if err != nil {
		return nil
	}
	targetIpnet, err := prefix.IPNet()
	if err != nil {
		return nil
	}
	for _, p := range prefixes {
		ipnet, err := p.IPNet()
		if err != nil {
			return nil
		}
		if ipnet.Contains(targetIpnet.IP) {
			return p
		}
	}
	return nil
}

// NewPrefix create a new Prefix from a string notation.
func (i *Ipamer) newPrefix(cidr string) (*Prefix, error) {
	_, _, err := net.ParseCIDR(cidr)
	if err != nil {
		return nil, fmt.Errorf("unable to parse cidr:%s %v", cidr, err)
	}
	p := &Prefix{
		Cidr:                   cidr,
		IPs:                    make(map[string]IP),
		AvailableChildPrefixes: make(map[string]Prefix),
		AcquiredChildPrefixes:  make(map[string]Prefix),
	}

	broadcast, err := p.broadcast()
	if err != nil {
		return nil, err
	}
	// First IP in the prefix and Broadcast is blocked.
	network, err := p.Network()
	if err != nil {
		return nil, err
	}
	p.IPs[network.String()] = IP{IP: network}
	p.IPs[broadcast.IP.String()] = *broadcast

	return p, nil
}

func (p *Prefix) String() string {
	return fmt.Sprintf("CIDR:%s IPNet:%v Network:%v", p.Cidr, p.IPNet, p.Network)
}

func (p *Prefix) IPNet() (*net.IPNet, error) {
	_, ipnet, err := net.ParseCIDR(p.Cidr)
	return ipnet, err
}
func (p *Prefix) Network() (net.IP, error) {
	ip, ipnet, err := net.ParseCIDR(p.Cidr)
	if err != nil {
		return nil, err
	}
	return ip.Mask(ipnet.Mask), nil
}
