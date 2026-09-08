package hub

import (
	"encoding/json"
	"errors"
	"net/netip"
	"reflect"
	"slices"
)

func match(left, right any) any {
	return map[string]any{"match": map[string]any{"op": "==", "left": left, "right": right}}
}

func payload(protocol, field string) any {
	return map[string]any{"payload": map[string]any{"protocol": protocol, "field": field}}
}

func prefix(value string) any {
	p := netip.MustParsePrefix(value)
	return map[string]any{"prefix": map[string]any{"addr": p.Addr().String(), "len": p.Bits()}}
}

func expectedNFT(c Config) []map[string]any {
	ports := []int{c.SQLPort, c.RemotesPort}
	slices.Sort(ports)
	port := match(payload("tcp", "dport"), map[string]any{"set": ports})
	iface := func(name string) any { return match(map[string]any{"meta": map[string]any{"key": "iifname"}}, name) }
	accept := map[string]any{"accept": nil}
	objects := []map[string]any{
		{"table": map[string]any{"family": "inet", "name": tableName}},
		{"chain": map[string]any{"family": "inet", "table": tableName, "name": "ingress",
			"type": "filter", "hook": "input", "prio": -10, "policy": "accept"}},
	}
	for _, expr := range [][]any{
		{iface("lo"), port, accept},
		{iface(c.Interface), match(payload("ip", "saddr"), prefix(c.AllowIPv4)), match(payload("ip", "daddr"), c.IPv4), port, accept},
		{iface(c.Interface), match(payload("ip6", "saddr"), prefix(c.AllowIPv6)), match(payload("ip6", "daddr"), c.IPv6), port, accept},
		{port, map[string]any{"drop": nil}},
	} {
		objects = append(objects, map[string]any{"rule": map[string]any{"family": "inet", "table": tableName, "chain": "ingress", "expr": expr}})
	}
	return objects
}

// checkNFT recognizes only this generated, applied inet table, including the
// hook and ordered rules. Extra rules, dormant flags, sets, chains and unknown
// semantics refuse. Other tables are neither inspected nor changed. A drop in
// this input base chain cannot be overridden by an accept in another table.
func checkNFT(raw []byte, c Config) error {
	var doc map[string]json.RawMessage
	if json.Unmarshal(raw, &doc) != nil || len(doc) != 1 {
		return errors.New("unreadable or malformed applied nftables JSON")
	}
	var actual []map[string]any
	if json.Unmarshal(doc["nftables"], &actual) != nil {
		return errors.New("missing applied nftables objects")
	}
	if len(actual) > 0 && len(actual[0]) == 1 && actual[0]["metainfo"] != nil {
		actual = actual[1:]
	}
	for _, object := range actual {
		if len(object) != 1 {
			return errors.New("unexpected applied nftables object")
		}
		for kind, value := range object {
			fields, ok := value.(map[string]any)
			if !ok || (kind != "table" && kind != "chain" && kind != "rule") {
				return errors.New("unexpected applied nftables object type")
			}
			delete(fields, "handle") // Kernel-assigned identity has no policy meaning.
		}
	}
	want, err := json.Marshal(expectedNFT(c))
	if err != nil {
		return err
	}
	var expected []map[string]any
	if err := json.Unmarshal(want, &expected); err != nil {
		return err
	}
	if !reflect.DeepEqual(actual, expected) {
		return errors.New("applied inet memdolt_hub does not exactly enforce the generated IPv4/IPv6 private ingress rules")
	}
	return nil
}
