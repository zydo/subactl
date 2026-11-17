module github.com/zydo/subactl/examples/mini-agent

go 1.26.0

require (
	github.com/zydo/subactl v0.0.0
	golang.org/x/term v0.48.0
)

require golang.org/x/sys v0.50.0 // indirect

replace github.com/zydo/subactl => ../..
