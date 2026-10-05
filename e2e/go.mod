// The browser tests live in a module of their own, so that the program's
// module does not depend on a browser driver.
module github.com/IsaacFW/reflectingpool/e2e

go 1.27

require (
	github.com/chromedp/cdproto v0.157.4
	github.com/chromedp/chromedp v0.19.1
)
