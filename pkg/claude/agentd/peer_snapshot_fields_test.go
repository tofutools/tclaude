package agentd

import (
	"reflect"
	"strings"
	"testing"
)

func TestPeerSnapshotFieldClassification(t *testing.T) {
	for typ, fields := range peerSnapshotFields {
		for i := 0; i < typ.NumField(); i++ {
			name := typ.Field(i).Name
			policy, ok := fields[name]
			if !ok {
				t.Errorf("%s.%s has no restricted peer classification (default denied)", typ.Name(), name)
				continue
			}
			if policy != "denied" && !allPeerProjectionFields[policy] {
				t.Errorf("%s.%s has unknown policy %q", typ.Name(), name, policy)
			}
		}
		for name := range fields {
			if _, ok := typ.FieldByName(name); !ok {
				t.Errorf("%s: stale field classification %s", typ.Name(), name)
			}
		}
	}
	// The types containing peer-visible rows must retain explicit classifiers.
	for _, typ := range []reflect.Type{reflect.TypeFor[snapshotPayload](), reflect.TypeFor[dashboardGroup](), reflect.TypeFor[dashboardMember](), reflect.TypeFor[dashboardAgent](), reflect.TypeFor[agentState](), reflect.TypeFor[taskRefView]()} {
		if _, ok := peerSnapshotFields[typ]; !ok {
			t.Errorf("unclassified snapshot type %s", typ.Name())
		}
	}
}

// RequiredDashboardSnapshotFieldsForTest lets external flow tests compare the
// real snapshot schema without treating populated optional fields as required.
func RequiredDashboardSnapshotFieldsForTest() []string {
	typ := reflect.TypeFor[snapshotPayload]()
	var keys []string
	for i := 0; i < typ.NumField(); i++ {
		tag := strings.Split(typ.Field(i).Tag.Get("json"), ",")
		if len(tag) == 1 && tag[0] != "" && tag[0] != "-" {
			keys = append(keys, tag[0])
		}
	}
	return keys
}
