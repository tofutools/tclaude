package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestGroupsLsAllNodesMergesPeersAndReportsUnreachable(t *testing.T) {
	saved := allNodesReaders
	t.Cleanup(func() { allNodesReaders = saved })
	allNodesReaders.status = func(out any) error {
		return json.Unmarshal([]byte(`{"instance_id":"inst_self","name":"desk","peers":[
			{"instance_id":"inst_forge","label":"forge","trusted":true},
			{"instance_id":"inst_lab","label":"lab","trusted":true},
			{"instance_id":"inst_stranger","name":"carol","trusted":false}]}`), out)
	}
	allNodesReaders.local = func(out any) error {
		return json.Unmarshal([]byte(`[{"name":"ops","members":3,"online":2}]`), out)
	}
	read := []string{}
	allNodesReaders.peerView = func(node, endpoint string, out any) error {
		read = append(read, node+"/"+endpoint)
		if node == "inst_lab" {
			return errors.New("peer is unreachable")
		}
		return json.Unmarshal([]byte(`{"groups":[{"name":"build","members":[{},{}],"online":1}]}`), out)
	}

	var stdout, stderr bytes.Buffer
	if rc := runGroupsLsAllNodes(true, &stdout, &stderr); rc != 1 {
		t.Fatalf("an unreachable peer must make the exit status nonzero: rc=%d stderr=%s", rc, stderr.String())
	}
	var got struct {
		Nodes  []allNodesNode  `json:"nodes"`
		Groups []allNodesGroup `json:"groups"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, g := range got.Groups {
		names = append(names, g.Name)
	}
	if strings.Join(names, ",") != "ops@desk,build@forge" {
		t.Fatalf("groups = %v", names)
	}
	if got.Groups[1].Members != 2 || got.Groups[1].NodeID != "inst_forge" {
		t.Fatalf("forge row = %+v", got.Groups[1])
	}
	if len(got.Nodes) != 3 || got.Nodes[2].Name != "lab" || got.Nodes[2].Error == "" {
		t.Fatalf("nodes = %+v", got.Nodes)
	}
	for _, r := range read {
		if strings.HasPrefix(r, "inst_stranger") {
			t.Fatal("an untrusted peer must never be read")
		}
	}

	stdout.Reset()
	if rc := runGroupsLsAllNodes(false, &stdout, &stderr); rc != 1 {
		t.Fatalf("table rc=%d", rc)
	}
	if !strings.Contains(stdout.String(), "build@forge") || !strings.Contains(stdout.String(), "lab: unreachable") {
		t.Fatalf("table output:\n%s", stdout.String())
	}
}
