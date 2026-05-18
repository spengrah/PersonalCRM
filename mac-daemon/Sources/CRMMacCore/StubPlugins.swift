// StubPlugins previously hosted no-op scheduler placeholders used
// by the PluginRegistry while real source plugins were being built
// out. All sources now have real plugins, so the stubs have been
// removed. This file is kept (empty namespace) so any out-of-tree
// consumers' import of CRMMacCore still resolves; future stubs land
// here as needed.
import Foundation
