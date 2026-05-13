// crm-mac entry point. swift-argument-parser dispatches to one of
// the subcommands defined in Commands/. The executable target stays
// thin — argument parsing only; every business behavior lives in
// CRMMacLifecycle so it can be tested without the CLI shim.
import ArgumentParser

CRMMacCommand.main()
