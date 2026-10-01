package main

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

// crPortOwners resolves each chassisredirect port to its chassis by name. A
// port that is bound nowhere, or to a chassis SB no longer lists, has no
// owner. A query that fails or does not parse is an error that names its table.
func TestCRPortOwners(t *testing.T) {
	t.Parallel()
	twoChassis := ovsTable([]string{"_uuid", "name"}, [][]any{
		{ovsUUID("ch-1"), "gateway-1"},
		{ovsUUID("ch-2"), "gateway-2"},
	})
	ports := func(rows ...[]any) string {
		return ovsTable([]string{"logical_port", "chassis"}, rows)
	}
	tests := []struct {
		name        string
		chassis     string
		chassisErr  error
		ports       string
		wantOwners  map[string]string
		wantChassis map[string]bool
		wantErr     string
		wantIs      error
	}{
		{
			name:    "one port per chassis and one unbound",
			chassis: twoChassis,
			ports: ports(
				[]any{"cr-lr0-public", ovsUUID("ch-1")},
				[]any{"cr-lr1-public", ovsUUID("ch-2")},
				[]any{"cr-lr2-public", ovsSet()},
			),
			wantOwners: map[string]string{
				"cr-lr0-public": "gateway-1", "cr-lr1-public": "gateway-2", "cr-lr2-public": "",
			},
			wantChassis: map[string]bool{"gateway-1": true, "gateway-2": true},
		},
		{
			name:        "empty tables",
			chassis:     ovsTable([]string{"_uuid", "name"}, nil),
			ports:       ports(),
			wantOwners:  map[string]string{},
			wantChassis: map[string]bool{},
		},
		{
			name:        "a port bound to a chassis SB no longer lists",
			chassis:     twoChassis,
			ports:       ports([]any{"cr-lr0-public", ovsUUID("ch-gone")}),
			wantOwners:  map[string]string{"cr-lr0-public": ""},
			wantChassis: map[string]bool{"gateway-1": true, "gateway-2": true},
		},
		{
			name:       "the Chassis query fails",
			chassisErr: errBoom,
			ports:      ports(),
			wantErr:    "read ovsdb table Chassis",
			wantIs:     errBoom,
		},
		{
			name:    "the Port_Binding answer is not JSON",
			chassis: twoChassis,
			ports:   "not json",
			wantErr: "parse ovsdb table Port_Binding",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cmd := &fakeCommander{respond: func(argv []string) (string, error) {
				line := strings.Join(argv, " ")
				switch {
				case strings.Contains(line, "list Chassis"):
					return tc.chassis, tc.chassisErr
				case strings.Contains(line, "find Port_Binding type=chassisredirect"):
					return tc.ports, nil
				}
				return "", nil
			}}

			owners, chassis, err := crPortOwners(t.Context(), newTestLab(cmd, newFakeClock()))

			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want one containing %q", err, tc.wantErr)
				}
				if tc.wantIs != nil && !errors.Is(err, tc.wantIs) {
					t.Fatalf("err = %v does not wrap %v", err, tc.wantIs)
				}
				return
			}
			if err != nil {
				t.Fatalf("crPortOwners: %v", err)
			}
			if !reflect.DeepEqual(owners, tc.wantOwners) {
				t.Errorf("owners = %#v, want %#v", owners, tc.wantOwners)
			}
			if !reflect.DeepEqual(chassis, tc.wantChassis) {
				t.Errorf("chassis = %#v, want %#v", chassis, tc.wantChassis)
			}
		})
	}
}

// upstreamSelected names, per prefix, the gateways the upstream router forwards
// over: the best path and its multipath set. A prefix it only holds unselected
// gateway paths for has no gateway, and a prefix no gateway announces is not
// listed.
func TestUpstreamSelected(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		out     string
		err     error
		want    map[string][]string
		wantErr string
		wantIs  error
	}{
		{
			name: "the best path and an unselected one",
			out: `{"routes":{"192.0.2.10/32":[
				{"bestpath":true,"nexthops":[{"ip":"100.64.2.2"}]},
				{"nexthops":[{"ip":"100.64.3.2"}]}]}}`,
			want: map[string][]string{"192.0.2.10": {"gateway-2"}},
		},
		{
			name: "an ECMP pair",
			out: `{"routes":{"192.0.2.10/32":[
				{"nexthops":[{"ip":"100.64.3.2"}],"multipath":true},
				{"bestpath":true,"nexthops":[{"ip":"100.64.2.2"}]}]}}`,
			want: map[string][]string{"192.0.2.10": {"gateway-2", "gateway-3"}},
		},
		{
			name: "no routes",
			out:  `{"routes":{}}`,
			want: map[string][]string{},
		},
		{
			name: "a document without a routes key",
			out:  `{"vrfName":"default"}`,
			want: map[string][]string{},
		},
		{
			name: "an unselected gateway path and a route no gateway announces",
			out: `{"routes":{
				"192.0.2.12/32":[{"nexthops":[{"ip":"100.64.1.2"}]}],
				"198.51.100.0/24":[{"bestpath":true,"peer":{"peerId":"100.64.3.2"}}],
				"203.0.113.0/24":[{"bestpath":true,"nexthops":[{"ip":"0.0.0.0"}]}]}}`,
			want: map[string][]string{"192.0.2.12": {}, "198.51.100.0/24": {"gateway-3"}},
		},
		{
			name:    "the upstream does not answer",
			err:     errBoom,
			wantErr: "read upstream bgp",
			wantIs:  errBoom,
		},
		{
			name:    "the answer is not JSON",
			out:     "not json",
			wantErr: "parse upstream bgp",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cmd := &fakeCommander{respond: func([]string) (string, error) { return tc.out, tc.err }}

			got, err := upstreamSelected(t.Context(), newTestLab(cmd, newFakeClock()))

			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want one containing %q", err, tc.wantErr)
				}
				if tc.wantIs != nil && !errors.Is(err, tc.wantIs) {
					t.Fatalf("err = %v does not wrap %v", err, tc.wantIs)
				}
				return
			}
			if err != nil {
				t.Fatalf("upstreamSelected: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("selected = %#v, want %#v", got, tc.want)
			}
			if !cmd.called("exec clab-ovn-e2e-upstream vtysh -c show bgp ipv4 unicast json") {
				t.Errorf("the selected paths were not read from the upstream router: %v", cmd.lines())
			}
		})
	}
}
