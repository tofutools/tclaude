package harness

// piCLIPackage is pi's npm package. The `pi` bin is dist/bundle/cli.js, two
// levels below the package root, started through `#!/usr/bin/env node`. Unlike
// Gemini's, pi's bundle loads its runtime dependencies (pi-ai, the TUI, jiti
// for extensions, undici, …) as installed packages, so a local install hoists
// several dozen beside it; a global install keeps them inside the package.
var piCLIPackage = nodeCLIPackage{
	bin: "pi", pkg: "@earendil-works/pi-coding-agent", display: "pi",
	capability: "pi_launch_executable", searchDepth: 4, depLimit: 256,
}

// ResolvePiLaunchExecutable resolves `pi` from PATH to its entry point, its
// Node interpreter and the paths both need, for a launch inside a
// constructed root (see GeminiLaunchExecutable for why).
func ResolvePiLaunchExecutable() (NodeLaunchExecutable, error) {
	return piCLIPackage.resolve()
}
