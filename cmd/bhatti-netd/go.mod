module github.com/sahil-shubham/bhatti/cmd/bhatti-netd

go 1.26.3

require (
	github.com/sahil-shubham/bhatti v0.0.0
	gvisor.dev/gvisor v0.0.0-20260701204157-69c2d17aea96
)

require (
	github.com/google/btree v1.1.2 // indirect
	golang.org/x/exp v0.0.0-20251023183803-a4bb9ffd2546 // indirect
	golang.org/x/sys v0.43.0 // indirect
	golang.org/x/time v0.15.0 // indirect
)

// netd reuses pkg/gateway (guard/proxy/secret/link) from the parent module.
replace github.com/sahil-shubham/bhatti => ../..
