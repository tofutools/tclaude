package agentd_test

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"os"
	"regexp"
	"strings"
)

// agentdTestPartitionEnv names a file listing the top-level tests (one name
// per line) this process runs. scripts/ci/agentd-tests.py splits the
// package's `-test.list` output across several concurrent processes of one
// test binary: most of the suite's wall time is spent waiting on fixtures,
// not computing, so one runner's cores fit several serial processes. A
// list file rather than a -test.run argument, because the alternation of a
// few thousand names exceeds the per-argument length limit (128 KiB on
// Linux).
const agentdTestPartitionEnv = "TCLAUDE_AGENTD_TEST_PARTITION"

// applyAgentdTestPartition narrows -test.run to the partition file's names.
// It unsets the variable before returning so that a test re-executing this
// binary for a helper child (exec os.Args[0] -test.run=^TestX$) does not have
// its own selection replaced by the whole partition.
func applyAgentdTestPartition() error {
	path, ok := os.LookupEnv(agentdTestPartitionEnv)
	if !ok {
		return nil
	}
	if err := os.Unsetenv(agentdTestPartitionEnv); err != nil {
		return err
	}
	flag.Parse()
	explicit := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "test.run" {
			explicit = true
		}
	})
	if explicit {
		return errors.New("-test.run and " + agentdTestPartitionEnv + " both select tests; pass one")
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	var names []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		if name := strings.TrimSpace(scanner.Text()); name != "" {
			names = append(names, regexp.QuoteMeta(name))
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if len(names) == 0 {
		return fmt.Errorf("%s: empty partition %s", agentdTestPartitionEnv, path)
	}
	return flag.Set("test.run", "^("+strings.Join(names, "|")+")$")
}
