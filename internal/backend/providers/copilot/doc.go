// Package copilot provides the cohesive terminal-authoritative GitHub Copilot
// CLI implementation for the replacement backend.
//
// New requires an executable and an absolute PrivateRoot. NativeHome defaults
// to PrivateRoot/native-home and, when overridden, must remain inside
// PrivateRoot. It is the durable provider-owned COPILOT_HOME shared by native
// sessions; terminal identities, observation spools, and execution action
// credentials remain separate resources. Before first use, setup may run the
// native `copilot login` with COPILOT_HOME set to that exact directory.
// Copilot's documented environment-token, OS-keychain, and GitHub CLI fallback
// authentication continue to apply; the provider never imports ambient
// ~/.copilot credentials.
package copilot
