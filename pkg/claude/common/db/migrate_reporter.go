package db

import "database/sql"

// migrationStep is one entry in the ordered schema-migration chain: applying
// it advances the DB from schema version-1 to version. The whole chain is
// walked in order and every step whose version exceeds the DB's current
// schema version is applied. Each apply func bumps schema_version to its own
// version as its final (transactional) step, so the chain is crash-safe: a
// re-run resumes at whichever version last committed.
type migrationStep struct {
	version int
	apply   func(*sql.DB) error
}

// migrationSteps is the ordered schema-migration chain, one entry per version
// bump from v1→v2 up to the head (currentVersion). Adding a migration means
// writing its migrateV{n-1}toV{n} func and appending {n, migrateV{n-1}toV{n}}
// here (keep it sorted, contiguous, and in lockstep with currentVersion). The
// migrate() loop and TestMigrationStepsAreContiguous enforce that shape.
var migrationSteps = []migrationStep{
	{2, migrateV1toV2},
	{3, migrateV2toV3},
	{4, migrateV3toV4},
	{5, migrateV4toV5},
	{6, migrateV5toV6},
	{7, migrateV6toV7},
	{8, migrateV7toV8},
	{9, migrateV8toV9},
	{10, migrateV9toV10},
	{11, migrateV10toV11},
	{12, migrateV11toV12},
	{13, migrateV12toV13},
	{14, migrateV13toV14},
	{15, migrateV14toV15},
	{16, migrateV15toV16},
	{17, migrateV16toV17},
	{18, migrateV17toV18},
	{19, migrateV18toV19},
	{20, migrateV19toV20},
	{21, migrateV20toV21},
	{22, migrateV21toV22},
	{23, migrateV22toV23},
	{24, migrateV23toV24},
	{25, migrateV24toV25},
	{26, migrateV25toV26},
	{27, migrateV26toV27},
	{28, migrateV27toV28},
	{29, migrateV28toV29},
	{30, migrateV29toV30},
	{31, migrateV30toV31},
	{32, migrateV31toV32},
	{33, migrateV32toV33},
	{34, migrateV33toV34},
	{35, migrateV34toV35},
	{36, migrateV35toV36},
	{37, migrateV36toV37},
	{38, migrateV37toV38},
	{39, migrateV38toV39},
	{40, migrateV39toV40},
	{41, migrateV40toV41},
	{42, migrateV41toV42},
	{43, migrateV42toV43},
	{44, migrateV43toV44},
	{45, migrateV44toV45},
	{46, migrateV45toV46},
	{47, migrateV46toV47},
	{48, migrateV47toV48},
	{49, migrateV48toV49},
	{50, migrateV49toV50},
	{51, migrateV50toV51},
	{52, migrateV51toV52},
	{53, migrateV52toV53},
	{54, migrateV53toV54},
	{55, migrateV54toV55},
	{56, migrateV55toV56},
	{57, migrateV56toV57},
	{58, migrateV57toV58},
	{59, migrateV58toV59},
	{60, migrateV59toV60},
	{61, migrateV60toV61},
	{62, migrateV61toV62},
	{63, migrateV62toV63},
	{64, migrateV63toV64},
	{65, migrateV64toV65},
	{66, migrateV65toV66},
	{67, migrateV66toV67},
	{68, migrateV67toV68},
	{69, migrateV68toV69},
	{70, migrateV69toV70},
	{71, migrateV70toV71},
	{72, migrateV71toV72},
	{73, migrateV72toV73},
	{74, migrateV73toV74},
	{75, migrateV74toV75},
	{76, migrateV75toV76},
	{77, migrateV76toV77},
	{78, migrateV77toV78},
	{79, migrateV78toV79},
	{80, migrateV79toV80},
	{81, migrateV80toV81},
	{82, migrateV81toV82},
	{83, migrateV82toV83},
	{84, migrateV83toV84},
	{85, migrateV84toV85},
	{86, migrateV85toV86},
	{87, migrateV86toV87},
	{88, migrateV87toV88},
	{89, migrateV88toV89},
	{90, migrateV89toV90},
	{91, migrateV90toV91},
	{92, migrateV91toV92},
	{93, migrateV92toV93},
	{94, migrateV93toV94},
	{95, migrateV94toV95},
	{96, migrateV95toV96},
	{97, migrateV96toV97},
	{98, migrateV97toV98},
	{99, migrateV98toV99},
	{100, migrateV99toV100},
	{101, migrateV100toV101},
	{102, migrateV101toV102},
	{103, migrateV102toV103},
	{104, migrateV103toV104},
	{105, migrateV104toV105},
	{106, migrateV105toV106},
	{107, migrateV106toV107},
	{108, migrateV107toV108},
	{109, migrateV108toV109},
	{110, migrateV109toV110},
	{111, migrateV110toV111},
	{112, migrateV111toV112},
	{113, migrateV112toV113},
	{114, migrateV113toV114},
}

// MigrationReporter carries optional callbacks that migrate() invokes as it
// applies the schema-migration chain, so a caller can surface progress to a
// human. agentd installs one at startup (see SetMigrationReporter) so the
// operator sees which migrations apply — and, crucially, which one FAILED —
// on the terminal before the daemon brings up its listeners. Ordinary CLI
// commands never install a reporter, so they migrate silently: this output is
// agentd-startup-only.
//
// A nil callback field is skipped. When the DB is already at head (or, rarely,
// past it) migrate() applies nothing and fires ONLY AlreadyCurrent — none of
// the Begin/Applying/Applied/Done bookends run — so a normal restart still
// emits a single "nothing to migrate" line instead of the earlier silence
// (which left an operator unable to tell a no-op restart from a migration that
// failed before it could report anything).
type MigrationReporter struct {
	// AlreadyCurrent fires once, INSTEAD of the whole Begin…Done sequence, when
	// migrate() finds no forward work: the DB is already at head (version ==
	// head) or, pathologically, past it (version > head — a newer binary wrote
	// the schema, so this older binary applies nothing and may not understand
	// it). version is the DB's actual schema version; head is the version this
	// binary knows (currentVersion), passed so the consumer can distinguish the
	// benign at-head case from the version > head anomaly without importing the
	// constant. This is the only callback that fires on a no-op restart.
	AlreadyCurrent func(version, head int)
	// Begin fires once before the first migration runs, when there is work to
	// do. from is the DB's current schema version (0 for a brand-new DB), to
	// the head version being migrated to.
	Begin func(from, to int)
	// Applying fires just before the migration that advances the schema to
	// version runs.
	Applying func(version int)
	// Applied fires after that migration commits successfully.
	Applied func(version int)
	// Failed fires if a migration returns an error; the chain then aborts and
	// migrate() returns that error. err is the migration's error.
	Failed func(version int, err error)
	// Done fires once after the whole chain succeeds, with the head version
	// reached.
	Done func(to int)
}

// migrationReporter is the process-wide reporter migrate() consults. nil (the
// default) means "migrate silently" — every CLI command. agentd sets it once
// at startup via SetMigrationReporter, before its first Open(). Written only
// at startup on the main goroutine before any concurrency, so it needs no
// lock (mirrors the other startup-resolved package vars).
var migrationReporter *MigrationReporter

// SetMigrationReporter installs (or, with nil, clears) the reporter migrate()
// calls as it applies schema migrations. Call it before the first Open(): the
// migration chain runs inside Open() the first time it finds an out-of-date
// DB, so a reporter installed after that open never fires. Intended for a
// single startup call from agentd; not safe to call concurrently with Open().
func SetMigrationReporter(r *MigrationReporter) {
	migrationReporter = r
}

// The report* helpers are nil-receiver- AND nil-field-safe so migrate() can
// call them unconditionally: a nil reporter (the CLI default) or an unset
// field is simply a no-op.

func (r *MigrationReporter) reportAlreadyCurrent(version, head int) {
	if r != nil && r.AlreadyCurrent != nil {
		r.AlreadyCurrent(version, head)
	}
}

func (r *MigrationReporter) reportBegin(from, to int) {
	if r != nil && r.Begin != nil {
		r.Begin(from, to)
	}
}

func (r *MigrationReporter) reportApplying(version int) {
	if r != nil && r.Applying != nil {
		r.Applying(version)
	}
}

func (r *MigrationReporter) reportApplied(version int) {
	if r != nil && r.Applied != nil {
		r.Applied(version)
	}
}

func (r *MigrationReporter) reportFailed(version int, err error) {
	if r != nil && r.Failed != nil {
		r.Failed(version, err)
	}
}

func (r *MigrationReporter) reportDone(to int) {
	if r != nil && r.Done != nil {
		r.Done(to)
	}
}
