package agentd

// Several agentd tests read fixtures straight out of jstest/ (the JSON shared
// with the Node suites, the Preact harness files), a directory PR CI's
// selector attributes to the jstest package. This edge puts jstest in agentd's
// test closure, so a change there selects agentd too. jstest has no code to
// link; the import is for the graph.
import _ "github.com/tofutools/tclaude/pkg/claude/agentd/jstest"
