module watershedtool

go 1.24

require github.com/jchv/go-webview2 v0.0.0-20260205173254-56598839c808

require (
	github.com/jchv/go-winloader v0.0.0-20250406163304-c1995be93bd1 // indirect
	golang.org/x/sys v0.0.0-20210218145245-beda7e5e158e // indirect
)

// golang.org is unreachable from some build environments; the same code is
// mirrored on GitHub. The module is vendored (vendor/) so builds need no
// network at all.
replace golang.org/x/sys => github.com/golang/sys v0.33.0
