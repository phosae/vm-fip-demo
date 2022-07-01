package testing

import (
	"bytes"
	"fmt"
	"strings"
	"time"

	"qiniu.com/qvirt/pkg/iptables"
)

// FakeIPTables is no-op implementation of iptables Interface.
type FakeIPTables struct {
	hasRandomFully bool
	protocol       iptables.Protocol

	Dump *IPTablesDump
}

// NewFake returns a no-op iptables.Interface
func NewFake() *FakeIPTables {
	f := &FakeIPTables{
		protocol: iptables.ProtocolIPv4,
		Dump: &IPTablesDump{
			Tables: []Table{
				{
					Name: iptables.TableNAT,
					Chains: []Chain{
						{Name: iptables.ChainPrerouting},
						{Name: iptables.ChainInput},
						{Name: iptables.ChainOutput},
						{Name: iptables.ChainPostrouting},
					},
				},
				{
					Name: iptables.TableFilter,
					Chains: []Chain{
						{Name: iptables.ChainInput},
						{Name: iptables.ChainForward},
						{Name: iptables.ChainOutput},
					},
				},
				{
					Name:   iptables.TableMangle,
					Chains: []Chain{},
				},
			},
		},
	}

	return f
}

// NewIPv6Fake returns a no-op iptables.Interface with IsIPv6() == true
func NewIPv6Fake() *FakeIPTables {
	f := NewFake()
	f.protocol = iptables.ProtocolIPv6
	return f
}

// SetHasRandomFully sets f's return value for HasRandomFully()
func (f *FakeIPTables) SetHasRandomFully(can bool) *FakeIPTables {
	f.hasRandomFully = can
	return f
}

// EnsureChain is part of iptables.Interface
func (f *FakeIPTables) EnsureChain(table iptables.Table, chain iptables.Chain) (bool, error) {
	t, err := f.Dump.GetTable(table)
	if err != nil {
		return false, err
	}
	if c, _ := f.Dump.GetChain(table, chain); c != nil {
		return true, nil
	}
	t.Chains = append(t.Chains, Chain{Name: chain})
	return false, nil
}

// FlushChain is part of iptables.Interface
func (f *FakeIPTables) FlushChain(table iptables.Table, chain iptables.Chain) error {
	if c, _ := f.Dump.GetChain(table, chain); c != nil {
		c.Rules = nil
	}
	return nil
}

// DeleteChain is part of iptables.Interface
func (f *FakeIPTables) DeleteChain(table iptables.Table, chain iptables.Chain) error {
	t, err := f.Dump.GetTable(table)
	if err != nil {
		return err
	}
	for i := range t.Chains {
		if t.Chains[i].Name == chain {
			t.Chains = append(t.Chains[:i], t.Chains[i+1:]...)
			return nil
		}
	}
	return nil
}

// ChainExists is part of iptables.Interface
func (f *FakeIPTables) ChainExists(table iptables.Table, chain iptables.Chain) (bool, error) {
	if _, err := f.Dump.GetTable(table); err != nil {
		return false, err
	}
	if c, _ := f.Dump.GetChain(table, chain); c != nil {
		return true, nil
	}
	return false, nil
}

// EnsureRule is part of iptables.Interface
func (f *FakeIPTables) EnsureRule(position iptables.RulePosition, table iptables.Table, chain iptables.Chain, args ...string) (bool, error) {
	c, err := f.Dump.GetChain(table, chain)
	if err != nil {
		return false, err
	}

	rule := "-A " + string(chain) + " " + strings.Join(args, " ")
	for _, r := range c.Rules {
		if r.Raw == rule {
			return true, nil
		}
	}

	parsed, err := ParseRule(rule, false)
	if err != nil {
		return false, err
	}

	if position == iptables.Append {
		c.Rules = append(c.Rules, parsed)
	} else {
		c.Rules = append([]*Rule{parsed}, c.Rules...)
	}
	return false, nil
}

// DeleteRule is part of iptables.Interface
func (f *FakeIPTables) DeleteRule(table iptables.Table, chain iptables.Chain, args ...string) error {
	c, err := f.Dump.GetChain(table, chain)
	if err != nil {
		return err
	}

	rule := "-A " + string(chain) + " " + strings.Join(args, " ")
	for i, r := range c.Rules {
		if r.Raw == rule {
			c.Rules = append(c.Rules[:i], c.Rules[i+1:]...)
			break
		}
	}
	return nil
}

// IsIPv6 is part of iptables.Interface
func (f *FakeIPTables) IsIPv6() bool {
	return f.protocol == iptables.ProtocolIPv6
}

// Protocol is part of iptables.Interface
func (f *FakeIPTables) Protocol() iptables.Protocol {
	return f.protocol
}

func (f *FakeIPTables) saveTable(table iptables.Table, buffer *bytes.Buffer) error {
	t, err := f.Dump.GetTable(table)
	if err != nil {
		return err
	}

	fmt.Fprintf(buffer, "*%s\n", table)
	for _, c := range t.Chains {
		fmt.Fprintf(buffer, ":%s - [%d:%d]\n", c.Name, c.Packets, c.Bytes)
	}
	for _, c := range t.Chains {
		for _, r := range c.Rules {
			fmt.Fprintf(buffer, "%s\n", r.Raw)
		}
	}
	fmt.Fprintf(buffer, "COMMIT\n")
	return nil
}

// SaveInto is part of iptables.Interface
func (f *FakeIPTables) SaveInto(table iptables.Table, buffer *bytes.Buffer) error {
	if table == "" {
		// As a secret extension to the API, FakeIPTables treats table="" as
		// meaning "all tables"
		for i := range f.Dump.Tables {
			err := f.saveTable(f.Dump.Tables[i].Name, buffer)
			if err != nil {
				return err
			}
		}
		return nil
	}

	return f.saveTable(table, buffer)
}

func (f *FakeIPTables) restoreTable(newTable *Table, flush iptables.FlushFlag, counters iptables.RestoreCountersFlag) error {
	oldTable, err := f.Dump.GetTable(newTable.Name)
	if err != nil {
		return err
	}

	if flush == iptables.FlushTables {
		oldTable.Chains = make([]Chain, 0, len(newTable.Chains))
	}

	for _, newChain := range newTable.Chains {
		oldChain, _ := f.Dump.GetChain(newTable.Name, newChain.Name)
		switch {
		case oldChain == nil && newChain.Deleted:
			// no-op
		case oldChain == nil && !newChain.Deleted:
			oldTable.Chains = append(oldTable.Chains, newChain)
		case oldChain != nil && newChain.Deleted:
			// FIXME: should make sure chain is not referenced from other jumps
			_ = f.DeleteChain(newTable.Name, newChain.Name)
		case oldChain != nil && !newChain.Deleted:
			// replace old data with new
			oldChain.Rules = newChain.Rules
			if counters == iptables.RestoreCounters {
				oldChain.Packets = newChain.Packets
				oldChain.Bytes = newChain.Bytes
			}
		}
	}
	return nil
}

// Restore is part of iptables.Interface
func (f *FakeIPTables) Restore(table iptables.Table, data []byte, flush iptables.FlushFlag, counters iptables.RestoreCountersFlag) error {
	dump, err := ParseIPTablesDump(string(data))
	if err != nil {
		return err
	}

	newTable, err := dump.GetTable(table)
	if err != nil {
		return err
	}

	return f.restoreTable(newTable, flush, counters)
}

// RestoreAll is part of iptables.Interface
func (f *FakeIPTables) RestoreAll(data []byte, flush iptables.FlushFlag, counters iptables.RestoreCountersFlag) error {
	dump, err := ParseIPTablesDump(string(data))
	if err != nil {
		return err
	}

	for i := range dump.Tables {
		err = f.restoreTable(&dump.Tables[i], flush, counters)
		if err != nil {
			return err
		}
	}
	return nil
}

// Monitor is part of iptables.Interface
func (f *FakeIPTables) Monitor(canary iptables.Chain, tables []iptables.Table, reloadFunc func(), interval time.Duration, stopCh <-chan struct{}) {
}

// HasRandomFully is part of iptables.Interface
func (f *FakeIPTables) HasRandomFully() bool {
	return f.hasRandomFully
}

func (f *FakeIPTables) Present() bool {
	return true
}

var _ = iptables.Interface(&FakeIPTables{})
