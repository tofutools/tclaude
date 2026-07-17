package pathv1

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"sort"
	"testing"
)

// alternateField deliberately does not call Encoder or any production
// identity helper. It is the independent reproduction for published vectors.
type alternateField func([]byte) []byte

func alternateString(value string) alternateField {
	return func(out []byte) []byte {
		var size [4]byte
		binary.BigEndian.PutUint32(size[:], uint32(len(value)))
		out = append(out, size[:]...)
		return append(out, value...)
	}
}
func alternateUint(value uint64) alternateField {
	return func(out []byte) []byte {
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], value)
		return append(out, b[:]...)
	}
}
func alternateStringList(values ...string) alternateField {
	return func(out []byte) []byte {
		values = append([]string(nil), values...)
		sort.Strings(values)
		values = compactAlternate(values)
		var count [4]byte
		binary.BigEndian.PutUint32(count[:], uint32(len(values)))
		out = append(out, count[:]...)
		for _, value := range values {
			out = alternateString(value)(out)
		}
		return out
	}
}
func alternateTupleList(tuples ...[]alternateField) alternateField {
	return func(out []byte) []byte {
		encoded := make([]string, 0, len(tuples))
		for _, tuple := range tuples {
			var b []byte
			for _, field := range tuple {
				b = field(b)
			}
			encoded = append(encoded, string(b))
		}
		sort.Strings(encoded)
		encoded = compactAlternate(encoded)
		var count [4]byte
		binary.BigEndian.PutUint32(count[:], uint32(len(encoded)))
		out = append(out, count[:]...)
		for _, tuple := range encoded {
			out = append(out, tuple...)
		}
		return out
	}
}
func alternateOrderedTupleList(tuples ...[]alternateField) alternateField {
	return func(out []byte) []byte {
		var count [4]byte
		binary.BigEndian.PutUint32(count[:], uint32(len(tuples)))
		out = append(out, count[:]...)
		for _, tuple := range tuples {
			for _, field := range tuple {
				out = field(out)
			}
		}
		return out
	}
}
func compactAlternate[T comparable](values []T) []T {
	if len(values) == 0 {
		return values
	}
	n := 1
	for _, v := range values[1:] {
		if v != values[n-1] {
			values[n] = v
			n++
		}
	}
	return values[:n]
}
func alternateHash(tag string, fields ...alternateField) string {
	b := append([]byte("tclaude.process/"), tag...)
	b = append(b, 0)
	for _, field := range fields {
		b = field(b)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func mustIdentity(t *testing.T) func(string, error) string {
	t.Helper()
	return func(value string, err error) string {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
}

func TestPublishedGoldenVectors(t *testing.T) {
	t.Parallel()
	timestamp := "2026-07-15T00:00:00.123456789Z"
	admin := PathV1AdminRecord{RunID: "run-7", EventSeq: 12, OriginalArrayIndex: 0, AdminType: "branch_skip", Actor: "human:johan", ReasonCode: "waived", EvidenceRef: "ticket-9", Timestamp: timestamp, ResolutionDigest: "resolution-d"}
	commands := []struct {
		name, want string
		identity   CommandIdentity
	}{
		{"command_empty", "d445a2b6a0a6061b67de42f042e6e91c4e4e5565a9a84e6eb47da0b12e3bc12a", CommandIdentity{RunID: "run-7", PayloadSchema: 1}},
		{"command_initialize", "37396f31d2b1285ca9b29f698bf9081dba313ae4cfd9b28ae1fa33115f12e7b0", CommandIdentity{RunID: "run-7", Kind: CommandInitializeRouting, PayloadSchema: 1, InputDigest: "legacy-d", PlanDigest: "routing-d"}},
		{"command_perform", "5cba5ec15a5e017ba0838aaacd97f88f637c506827ce963453d487c9fdbd18f0", CommandIdentity{RunID: "run-7", Kind: CommandPerformAttempt, PayloadSchema: 1, SourceActivationID: "act-a", SourceGeneration: 1, Attempt: 2, PlanDigest: "performer-d"}},
		{"command_settle", "899c0bb3ab7cc797d64d7b01872b3dcab843f48ea592a192759c9de821dfa839", CommandIdentity{RunID: "run-7", Kind: CommandSettleAttempt, PayloadSchema: 1, SourceActivationID: "act-a", SourceGeneration: 1, Attempt: 2, InputDigest: "sourcecmd-d", PlanDigest: "observe-d", ResultCode: "pass"}},
		{"command_route", "3dc70fe9515c2ac8eef1cdd142858aeec18b8de9e6a2ec09dae3716643c67ae0", CommandIdentity{RunID: "run-7", Kind: CommandRoutePaths, PayloadSchema: 1, SourceActivationID: "act-a", SourceGeneration: 1, SourcePathID: "path-o", Attempt: 2, InputDigest: "settle-d", CauseDigest: "cause-d", PlanDigest: "edges-d", ResultCode: "exclusive/pass"}},
		{"command_activate", "34f71c3258d1d9348dc510ecdd7d976ccd21417a0fb4c2a5e0922fbe6aeccf26", CommandIdentity{RunID: "run-7", Kind: CommandActivateGeneration, PayloadSchema: 1, TargetReservationID: "res-b", TargetGeneration: 1, InputDigest: "fold-d", CauseDigest: "cause-d", PlanDigest: "activate-d"}},
		{"command_propagate", "7e70ad56098fd88217b6b2ca72276e248f8910a0e2ecf9b514aa8f107fa66691", CommandIdentity{RunID: "run-7", Kind: CommandPropagateCandidateClosure, PayloadSchema: 1, SourcePathID: "path-o", TargetReservationID: "res-b", TargetGeneration: 1, InputDigest: "fold-d", CauseDigest: "cause-d", PlanDigest: "closure-d"}},
		{"command_sink", "765e6a0497221e24c89d7e2d87927b8bc1f6f56dda791c5ac1f5a5ef09ed5614", CommandIdentity{RunID: "run-7", Kind: CommandSettleDetachedSink, PayloadSchema: 1, SourcePathID: "loser-p", TargetReservationID: "res-b", TargetGeneration: 1, InputDigest: "detachset-d", CauseDigest: "cause-d", PlanDigest: "sink-plan-d", ResultCode: "detached"}},
		{"command_complete", "a607e985ba74212f1ca04bf8b4fe42634e62572facf86433e3d5c576af3c9c2e", CommandIdentity{RunID: "run-7", Kind: CommandCompleteRun, PayloadSchema: 1, InputDigest: "aggregate-d", PlanDigest: "activecmd-d", ResultCode: "completed"}},
	}

	vectors := []struct{ name, want, primary, alternate string }{
		{"cause_set_empty", "b484da6756921023305d0ca5072cdea2830f8bf4af670623a5a8cad26c3fff98", mustIdentity(t)(CauseSetIdentity(nil)), alternateHash("cause-set/v1", alternateStringList())},
		{"edge", "5a4b9454e49cd3ce1a090c81e3dd9f5ca3ec7f0db477a69d8f4e022d3b983f68", mustIdentity(t)(EdgeIdentity("tmpl@sha256:abc", "fork", "pass", "join")), alternateHash("edge/v1", alternateString("tmpl@sha256:abc"), alternateString("fork"), alternateString("pass"), alternateString("join"))},
		{"scope", "ad64ef94b666e875035d1b34df5f3ddfca9feff4ec32a9f03be38c985533bd0c", mustIdentity(t)(ScopeIdentity("run-7", "", "", "start-a", "start-p", 1)), alternateHash("scope/v1", alternateString("run-7"), alternateString(""), alternateString(""), alternateString("start-a"), alternateString("start-p"), alternateUint(1))},
		{"reservation", "18ff2ae0a18b2018bffc68b2f3ad721c0f6a2b1df071ec549be6b673bfd7f163", mustIdentity(t)(ReservationIdentity("run-7", "join", "scope-s", "edge-e", 1)), alternateHash("reservation/v1", alternateString("run-7"), alternateString("join"), alternateString("scope-s"), alternateString("edge-e"), alternateUint(1))},
		{"candidate", "2dd1828611efe633bac740f7fbdd8ff5f6290aa99d8d9865185217a6eb5f7e96", mustIdentity(t)(CandidateIdentity("res-b", CandidateScopeBranch, "edge-e")), alternateHash("candidate/v1", alternateString("res-b"), alternateString("scope_branch"), alternateString("edge-e"))},
		{"route_slot", "a2c9e51cbd9d7fa62108041b0c76c880c0a2f5312d8abae7cd10c6bd2c43601c", mustIdentity(t)(PossibleSlotIdentity("res-b", "cand-c", "source", "edge-e", "scope-s", "edge-e", 1)), alternateHash("route-slot/v1", alternateString("res-b"), alternateString("cand-c"), alternateString("source"), alternateString("edge-e"), alternateString("scope-s"), alternateString("edge-e"), alternateUint(1))},
		{"input_empty", "5296ab75231af09958f5d8b4366f84f333b53aea08fbf9fda294ac8333d7ddb6", mustIdentity(t)(InputSetIdentity(nil)), alternateHash("activation-input-set/v1", alternateStringList())},
		{"input_nonempty", "9fbe9b3b94287bace166a0e7ef53aa1ca0c468923f1deaddbabd19b7a797705b", mustIdentity(t)(InputSetIdentity([]string{"p2", "p1"})), alternateHash("activation-input-set/v1", alternateStringList("p2", "p1"))},
		{"activation", "b1b62c10f56bff2136ece7eab0e9d0cca0bc1429ee52912663dc89c60a1ae69d", mustIdentity(t)(ActivationIdentity("run-7", "res-b", 1, "input-d")), alternateHash("activation/v1", alternateString("run-7"), alternateString("res-b"), alternateUint(1), alternateString("input-d"))},
		{"activation_output", "8c880b0d9657e803374525b7e17422e7a093af5ce828300d5630116976557416", mustIdentity(t)(ActivationOutputIdentity("act-a", 1)), alternateHash("activation-output/v1", alternateString("act-a"), alternateUint(1))},
		{"edge_token", "e585f4ec3b10711630ddba8f381a81a96a8674d70011b58a4809a2fad4e4deee", mustIdentity(t)(EdgePathIdentity("act-a", "path-o", "edge-e", "res-b", "cand-c")), alternateHash("edge-token/v1", alternateString("act-a"), alternateString("path-o"), alternateString("edge-e"), alternateString("res-b"), alternateString("cand-c"))},
		{"impossible_edge", "3aff0687340ba3d5f443095c80e0647f4cddee32c36a1b9488585a8e6b9ce3e7", mustIdentity(t)(ImpossibleEdgePathIdentity("cause-d", "edge-e", "res-b")), alternateHash("impossible-edge-token/v1", alternateString("cause-d"), alternateString("edge-e"), alternateString("res-b"))},
		{"arrival", "2c94035ed8cfce26f4cfedd369eb454288b5eb0eb68729f829f5ac87e3f7056d", mustIdentity(t)(ArrivalIdentity("edge-p", "res-b", "cand-c")), alternateHash("arrival/v1", alternateString("edge-p"), alternateString("res-b"), alternateString("cand-c"))},
		{"closure_key", "78c4e8a99b92c9d18dbc888f59851ceec8b7c0f919ca44f79da58ffed3afa212", mustIdentity(t)(CandidateClosureKeyIdentity("res-b", "cand-c")), alternateHash("candidate-closure-key/v1", alternateString("res-b"), alternateString("cand-c"))},
		{"cause", "04e2e758a18b35322c57719d1e016a321858c887c67a258e949de2544703c54b", mustIdentity(t)(CauseIdentity("path-o", TerminalFailed, "performer_failed", "act-a", "cmd-x", "", 9)), alternateHash("candidate-cause/v1", alternateString("path-o"), alternateString("failed"), alternateString("performer_failed"), alternateString("act-a"), alternateString("cmd-x"), alternateString(""), alternateUint(9))},
		{"cause_set", "c591f2e737139b498a520151563a39f9c67d62152b2701f8c805652c9d242c22", mustIdentity(t)(CauseSetIdentity([]string{"c2", "c1"})), alternateHash("cause-set/v1", alternateStringList("c2", "c1"))},
		{"closure", "9f86fcb881ceac3bb003915b4aff42672182a749e8300d96ff9679fe383405de", mustIdentity(t)(CandidateClosureIdentity("res-b", "cand-c", TerminalFailed, "cause-d")), alternateHash("candidate-closure/v1", alternateString("res-b"), alternateString("cand-c"), alternateString("failed"), alternateString("cause-d"))},
		{"lineage", "508af37e054365e2184286757d2f79c5e4420dedfa02d9ddde132fee3f5e74ff", mustIdentity(t)(CandidateLineageIdentity("lineage-parent", "res-b", "cand-c")), alternateHash("candidate-lineage/v1", alternateString("lineage-parent"), alternateString("res-b"), alternateString("cand-c"))},
		{"detachment_key", "5a760e871f61f6b2210b809871b54123f74cfc4953c9cc5f518f904bb0d2335f", mustIdentity(t)(DetachmentKeyIdentity("res-b", "cand-c")), alternateHash("detachment-key/v1", alternateString("res-b"), alternateString("cand-c"))},
		{"detachment", "1a1f2628ae837ffbd3c276a7668d3db730928d3ed7f86dbc32878703bd985c49", mustIdentity(t)(DetachmentIdentity("res-b", "cand-c", "winner-p", 11)), alternateHash("detachment/v1", alternateString("res-b"), alternateString("cand-c"), alternateString("winner-p"), alternateUint(11))},
		{"detachment_set", "1499abfa6038ff3b36c867af3a80028d6c5cd5788b980bbdac09ca664b919ebb", mustIdentity(t)(DetachmentSetIdentity("set-parent", "detach-d")), alternateHash("detachment-set/v1", alternateString("set-parent"), alternateString("detach-d"))},
		{"attempt", "a1aad17b6206534abf418f1d46e8dbdc5745563a57941d9e1369966e5974fe37", mustIdentity(t)(AttemptIdentity("run-7", "act-a", 2)), alternateHash("attempt/v1", alternateString("run-7"), alternateString("act-a"), alternateUint(2))},
		{"wait", "01e0abc320a26f67810dacc9dd0613947ec292dc175d04a30f9b02159db1bae9", mustIdentity(t)(WaitIdentity("run-7", "act-a", 2, "approval")), alternateHash("wait/v1", alternateString("run-7"), alternateString("act-a"), alternateUint(2), alternateString("approval"))},
		{"timer", "9d7e6d96885c6c82ffab9a35c9b27a81b4dd3b9c99c7837e61cded7e1899f272", mustIdentity(t)(TimerIdentity("run-7", "act-a", 2, "cmd-x")), alternateHash("timer/v1", alternateString("run-7"), alternateString("act-a"), alternateUint(2), alternateString("cmd-x"))},
		{"contact", "f1b35782a391d457d963a61f36f081a762dda56d6a675eb233fd669646d55ba1", mustIdentity(t)(ContactIdentity("run-7", "act-a", 2, "human:johan")), alternateHash("contact/v1", alternateString("run-7"), alternateString("act-a"), alternateUint(2), alternateString("human:johan"))},
		{"obligation", "9f3b41dc4767e46494a7616ea737daae740d29d7c516e158bacedc169a6ee2d8", mustIdentity(t)(ObligationIdentity("run-7", "act-a", 2, "approval", "human:johan")), alternateHash("obligation/v1", alternateString("run-7"), alternateString("act-a"), alternateUint(2), alternateString("approval"), alternateString("human:johan"))},
		{"block", "ac25c139cdaa21b48c436cfd43e3e0eb572a39e505c459cdcf519ff1204d53f1", mustIdentity(t)(BlockIdentity("run-7", "act-a", 2)), alternateHash("block/v1", alternateString("run-7"), alternateString("act-a"), alternateUint(2))},
		{"disposition", "fee13d63a10301872edef935ca63c952b05dc65f613bed78d5149ed393938ccc", mustIdentity(t)(DispositionReceiptIdentity("path-o", PathArrived, PathConsumed, "join_non_success", "cmd-x", "", 12)), alternateHash("disposition/v1", alternateString("path-o"), alternateString("arrived"), alternateString("consumed"), alternateString("join_non_success"), alternateString("cmd-x"), alternateString(""), alternateUint(12))},
		{"activation_receipt", "a9bfbb062843970c33ddb19860c4238801b64b1304d5a0f98d5cf65781c28cf1", mustIdentity(t)(ActivationReceiptIdentity("act-a", "res-b", "input-d", "path-o", "cmd-x", 12)), alternateHash("activation-receipt/v1", alternateString("act-a"), alternateString("res-b"), alternateString("input-d"), alternateString("path-o"), alternateString("cmd-x"), alternateUint(12))},
		{"propagation_plan", "5023f501363bb42e4e8cb9dadcaf9656d08a39feadc9205a10b9202b899aeed2", mustIdentity(t)(PropagationPlanIdentity("res-b", "cand-c", "cause-d", 0, []string{"key-a", "key-b"})), alternateHash("propagation-plan/v1", alternateString("res-b"), alternateString("cand-c"), alternateString("cause-d"), alternateUint(0), alternateStringList("key-b", "key-a"))},
		{"propagation_intent", "e714f2717d330c87843668e1ff3ea01b24d2d359801cbba11777324be545520a", mustIdentity(t)(PropagationIntentIdentity("cause-d", 0, "plan-d")), alternateHash("propagation-intent/v1", alternateString("cause-d"), alternateUint(0), alternateString("plan-d"))},
		{"candidate_fold", "7cd993cf837436660f79eb4c76d27e3a6d64fc745674c79d905c81231d07f255", mustIdentity(t)(CandidateFoldIdentity([]CandidateFoldEntry{{"cand-b", "failed", "closure-b"}, {"cand-a", "arrived", "path-a"}})), alternateHash("candidate-fold/v1", alternateTupleList([]alternateField{alternateString("cand-b"), alternateString("failed"), alternateString("closure-b")}, []alternateField{alternateString("cand-a"), alternateString("arrived"), alternateString("path-a")}))},
		{"active_command_empty", "1c776baca9ecc06a768d259f54acfa6d1aa4c46c24fa05e2a6e9b3196285e607", mustIdentity(t)(ActiveCommandIdentity(nil)), alternateHash("active-command-set/v1", alternateStringList())},
		{"checkpoint", "8185146d28a339658d271bae9b4f96a03f5f58d04e526ae8e6ab55234e3d13a8", mustIdentity(t)(CheckpointIdentity("running", 42, "log-d", []byte("{}"))), alternateHash("checkpoint/v1", alternateString("running"), alternateUint(42), alternateString("log-d"), alternateString("{}"))},
		{"path_fold", "ffd03c06b9fde14f6ff5f2252079e6a5a00ee52f3244dc29d0f05fde6a483806", mustIdentity(t)(PathFoldIdentity([]PathFoldEntry{{"p2", PathFailed, 8}, {"p1", PathEnded, 7}})), alternateHash("aggregate-paths/v1", alternateTupleList([]alternateField{alternateString("p2"), alternateString("failed"), alternateUint(8)}, []alternateField{alternateString("p1"), alternateString("ended"), alternateUint(7)}))},
		{"reservation_fold", "caa693eca50cc07fb23202565ac2eae2ef80d9c80e581697e2fd8a204129a63e", mustIdentity(t)(ReservationFoldIdentity([]ReservationFoldEntry{{"r2", ReservationClosedNoActivation, 8}, {"r1", ReservationActivated, 7}})), alternateHash("aggregate-reservations/v1", alternateTupleList([]alternateField{alternateString("r2"), alternateString("closed_no_activation"), alternateUint(8)}, []alternateField{alternateString("r1"), alternateString("activated"), alternateUint(7)}))},
		{"propagation_fold", "493dc7a063bb5d7d4515b708ab618570c4f4c8c178c76d074ff14a5b09a1f423", mustIdentity(t)(PropagationFoldIdentity([]PropagationFoldEntry{{"i2", PropagationComplete, 4}, {"i1", PropagationPending, 2}})), alternateHash("aggregate-propagation/v1", alternateTupleList([]alternateField{alternateString("i2"), alternateString("complete"), alternateUint(4)}, []alternateField{alternateString("i1"), alternateString("pending"), alternateUint(2)}))},
		{"side_effect_fold", "a95fd8c6898e1823d0e2af16ccd1b06cc6842ff52c3149f55e37659fb9d72fff", mustIdentity(t)(SideEffectFoldIdentity([]SideEffectFoldEntry{{SideEffectWait, "w1", "satisfied"}, {SideEffectCommand, "c1", "observed"}})), alternateHash("aggregate-side-effects/v1", alternateTupleList([]alternateField{alternateString("command"), alternateString("c1"), alternateString("observed")}, []alternateField{alternateString("wait"), alternateString("w1"), alternateString("satisfied")}))},
		{"aggregate", "40d05aaaf8189f47d47178c129aac63cdc7708c173190f53f270642136d4a8af", mustIdentity(t)(AggregateIdentity("run-7", "tmpl@sha256:abc", "checkpoint-d", "paths-d", "reservations-d", "propagation-d", "sideeffects-d", "causes-d")), alternateHash("aggregate/v1", alternateString("run-7"), alternateString("tmpl@sha256:abc"), alternateString("checkpoint-d"), alternateString("paths-d"), alternateString("reservations-d"), alternateString("propagation-d"), alternateString("sideeffects-d"), alternateString("causes-d"))},
		{"block_resolution", "e526f87547970d672d139969d3f9a3fbab6fd4d1a73aa5d11c0d43b5528e3acd", mustIdentity(t)(BlockResolutionIdentity(BlockResolution{"stage-a", 2, "skip", "human:johan", "waived", "ticket-9", timestamp})), alternateHash("block-resolution/v1", alternateString("stage-a"), alternateUint(2), alternateString("skip"), alternateString("human:johan"), alternateString("waived"), alternateString("ticket-9"), alternateString(timestamp))},
		{"admin", "634e590073c19b59117b8bd263fe4105faa3f195c18ee5aadfac116e79d85061", mustIdentity(t)(AdminRecordIdentity(admin)), alternateHash("admin-record/v1", alternateString("run-7"), alternateUint(12), alternateString("branch_skip"), alternateString("human:johan"), alternateString("waived"), alternateString("ticket-9"), alternateString("resolution-d"))},
		{"legacy_admin_0", "21da51f6e6b9cab4fd1f61c619a693121fd31b726eaa32033ea1e123f68f3fd3", mustIdentity(t)(LegacyAdminRecordIdentity(admin)), alternateHash("legacy-admin-record/v1", alternateString("run-7"), alternateUint(0), alternateString("branch_skip"), alternateString("human:johan"), alternateString("waived"), alternateString("ticket-9"), alternateString(timestamp), alternateString("resolution-d"))},
	}
	admin.OriginalArrayIndex = 1
	vectors = append(vectors, struct{ name, want, primary, alternate string }{"legacy_admin_1", "e9dabfc1c60cad9ca3e0b3eff0a00823362d83b4cefe25e4a7910f1f58316724", mustIdentity(t)(LegacyAdminRecordIdentity(admin)), alternateHash("legacy-admin-record/v1", alternateString("run-7"), alternateUint(1), alternateString("branch_skip"), alternateString("human:johan"), alternateString("waived"), alternateString("ticket-9"), alternateString(timestamp), alternateString("resolution-d"))})

	for _, command := range commands {
		identity := command.identity
		fields := []alternateField{alternateString(identity.RunID), alternateString(string(identity.Kind)), alternateUint(identity.PayloadSchema), alternateString(identity.SourceActivationID), alternateUint(identity.SourceGeneration), alternateString(identity.SourcePathID), alternateString(identity.TargetReservationID), alternateUint(identity.TargetGeneration), alternateUint(identity.Attempt), alternateString(identity.InputDigest), alternateString(identity.CauseDigest), alternateString(identity.PlanDigest), alternateString(identity.ResultCode)}
		vectors = append(vectors, struct{ name, want, primary, alternate string }{command.name, command.want, mustIdentity(t)(CommandIdentityDigest(identity)), alternateHash("command/v1", fields...)})
	}
	// The publication has 43 identity/admin/fold rows and nine command rows.
	if len(vectors) != 52 {
		t.Fatalf("vector count = %d, want 52", len(vectors))
	}
	for _, vector := range vectors {
		vector := vector
		t.Run(vector.name, func(t *testing.T) {
			t.Parallel()
			if vector.primary != vector.want {
				t.Errorf("primary = %s, want %s", vector.primary, vector.want)
			}
			if vector.alternate != vector.want {
				t.Errorf("independent = %s, want %s", vector.alternate, vector.want)
			}
		})
	}
}

func TestSideEffectFoldSortsEncodedTuplesNotRawFields(t *testing.T) {
	t.Parallel()
	entries := []SideEffectFoldEntry{
		{Kind: SideEffectCommand, ID: "c1", State: "observed"},
		{Kind: SideEffectWait, ID: "w1", State: "satisfied"},
	}
	want := alternateHash("aggregate-side-effects/v1", alternateTupleList(
		[]alternateField{alternateString("command"), alternateString("c1"), alternateString("observed")},
		[]alternateField{alternateString("wait"), alternateString("w1"), alternateString("satisfied")},
	))
	for _, input := range [][]SideEffectFoldEntry{entries, {entries[1], entries[0]}, {entries[0], entries[1], entries[0]}} {
		got, err := SideEffectFoldIdentity(input)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("fold = %s, want independently encoded %s", got, want)
		}
	}
	oldRawFieldOrder := alternateHash("aggregate-side-effects/v1", alternateOrderedTupleList(
		[]alternateField{alternateString("command"), alternateString("c1"), alternateString("observed")},
		[]alternateField{alternateString("wait"), alternateString("w1"), alternateString("satisfied")},
	))
	if want == oldRawFieldOrder {
		t.Fatal("fixture does not distinguish encoded-tuple order from raw-field order")
	}
}
