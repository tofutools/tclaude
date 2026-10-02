# Vendored xterm runtime

These classic browser assets back the terminal modal, dashboard terminal tab,
and standalone terminal pop-out. They are committed so terminal startup has no
runtime CDN dependency.

| Package | Version | Upstream file | Committed file | SHA-256 |
| --- | --- | --- | --- | --- |
| `@xterm/xterm` | 6.0.0 | `lib/xterm.js` | `xterm.min.js` | `e4d246be46c786a973e6d6eea46aef1eed56660b2a7f469a3b48b3738321646e` |
| `@xterm/xterm` | 6.0.0 | `css/xterm.css` | `xterm.min.css` | `99ae5d3f0651a557ba34946aeaa384c4ddd0e697ff205c7c1f5f955867063907` |
| `@xterm/addon-fit` | 0.11.0 | `lib/addon-fit.js` | `addon-fit.min.js` | `696bd2890cb91f96b6db0a83103d49088892ff440bf01d2da654c905cff7696c` |
| `@xterm/addon-web-links` | 0.12.0 | `lib/addon-web-links.js` | `addon-web-links.min.js` | `7b5d634522f0e93ef567b4f6d72d4b71f0a5e95070f079b004f7b945b7c4c9ab` |
| `@xterm/xterm` | 6.0.0 | `LICENSE` | `LICENSE.xterm` | `b569f629d00f2626a8100df2a1798210535621e42164dfd426a6fe5aac7b0ccd` |

All packages use the MIT license. The runtime files were fetched from the
version-pinned jsDelivr npm paths recorded in their headers; jsDelivr supplies
the minified CSS and provenance headers. The web-links file is the upstream
minified UMD body with its unavailable source-map trailer removed and the
local provenance/license header added.

To upgrade, use `npm pack --ignore-scripts <package>@<exact-version>` in a
temporary directory to inspect the upstream runtime and license, then fetch
the corresponding version-pinned `cdn.jsdelivr.net/npm/<package>@<version>/`
files into that directory. Apply the documented web-links header/trailer
normalization, copy only the four runtime files and current license here, and
update this table plus `TestDashboardXtermVendorAssets`. Run:

```bash
go test ./pkg/claude/agentd -run 'TestDashboard(XtermVendorAssets|Terminals_)' -count=1
go test ./...
```

Do not add npm metadata, a package cache, or a generated dependency tree.
