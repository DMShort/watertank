# Third-party code in the Watershed Tank Tool

Vendored under `src/vendor/` (licences included there):

| Module | Licence | Used for |
|---|---|---|
| github.com/jchv/go-webview2 | MIT | the tool's own window (Windows WebView2) |
| github.com/jchv/go-winloader | MIT | loading the WebView2 loader DLL |
| golang.org/x/sys (GitHub mirror) | BSD-3-Clause | Windows system calls |

Downloaded by the tool at run time, from the release:

| Tool | Licence | Used for |
|---|---|---|
| esptool (Espressif) | GPL-2.0 | flashing the node firmware (run as a separate program) |
