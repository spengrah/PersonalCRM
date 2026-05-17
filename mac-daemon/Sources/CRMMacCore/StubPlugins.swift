// StubPlugins previously hosted StubICloudContactsPlugin — a no-op
// scheduler placeholder used by the PluginRegistry while the real
// icloud_contacts source was unimplemented. PR8b lands the real
// plugin (CRMMacIcloudContactsSource.ICloudContactsSourcePlugin), so
// the stub is removed. This file is kept (empty namespace) so any
// out-of-tree consumers' import of CRMMacCore still resolves; future
// stubs land here as needed.
import Foundation
