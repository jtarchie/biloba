/*
Biloba builds on top of [chromedp] to bring stable, performant, browser automation to Ginkgo

Biloba embraces three principles:
  - Performance via parallelization
  - Stability via pragmatism
  - Conciseness via Ginkgo and Gomega

[chromedp]: https://github.com/chromedp/chromedp/
*/
package biloba

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "embed"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
)

type GinkgoTInterface interface {
	Helper()
	Fatal(args ...interface{})
	Fatalf(format string, args ...interface{})
	TempDir() string
	Logf(format string, args ...any)
	Failed() bool

	GinkgoRecover()
	DeferCleanup(args ...any)
	Print(args ...any)
	Printf(format string, args ...any)
	Println(a ...interface{})
	F(format string, args ...any) string
	Fi(indentation uint, format string, args ...any) string
	Fiw(indentation uint, maxWidth uint, format string, args ...any) string
	AddReportEntryVisibilityFailureOrVerbose(name string, args ...any)
	ParallelProcess() int
	ParallelTotal() int
	AttachProgressReporter(func() string) func()
}

type ChromeConnection struct {
	WebSocketURL string
}

func (gc ChromeConnection) encode() []byte {
	data, _ := json.Marshal(gc)
	return data
}

func StartingWindowSize(x int, y int) chromedp.ExecAllocatorOption {
	return chromedp.WindowSize(x, y)
}

func gooseConfigPath(process int) string {
	return fmt.Sprintf("./.biloba-config-%d", process)
}

func SpinUpChrome(ginkgoT GinkgoTInterface, options ...chromedp.ExecAllocatorOption) ChromeConnection {
	ginkgoT.Helper()
	tmp := ginkgoT.TempDir()
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		StartingWindowSize(1024, 768),
		chromedp.UserDataDir(tmp),
	)
	opts = append(opts, options...)

	allocCtx, cancel := chromedp.NewExecAllocator(context.Background(), opts...)
	ginkgoT.DeferCleanup(cancel)

	browserCtx, cancel := chromedp.NewContext(allocCtx)
	ginkgoT.DeferCleanup(cancel)

	err := chromedp.Run(browserCtx, chromedp.Evaluate("1", nil))
	if err != nil {
		ginkgoT.Fatalf("failed to spin up chrome: %w", err)
		return ChromeConnection{}
	}

	bs, err := os.ReadFile(filepath.Join(tmp, "DevToolsActivePort"))
	if err != nil {
		ginkgoT.Fatalf("failed to spin up chrome: %w", err)
		return ChromeConnection{}
	}

	components := strings.Split(string(bs), "\n")

	cc := ChromeConnection{
		WebSocketURL: fmt.Sprintf("ws://127.0.0.1:%s%s", components[0], components[1]),
	}

	os.WriteFile(gooseConfigPath(ginkgoT.ParallelProcess()), cc.encode(), 0744)
	ginkgoT.DeferCleanup(os.Remove, gooseConfigPath(ginkgoT.ParallelProcess()))

	return cc
}

type BilobaConfigOption func(*Biloba)

func BilobaConfigEnableDebugLogging() func(*Biloba) {
	return func(b *Biloba) {
		b.enableDebugLogging = true
	}
}

func BilobaConfigWithChromeConnection(cc ChromeConnection) func(*Biloba) {
	return func(b *Biloba) {
		b.ChromeConnection = cc
	}
}

func BilobaConfigDisableFailureScreenshots() func(*Biloba) {
	return func(b *Biloba) {
		b.disableFailureScreenshots = true
	}
}

func BilobaConfigDisableProgressReportScreenshots() func(*Biloba) {
	return func(b *Biloba) {
		b.disableProgressReportScreenshots = true
	}
}

func ConnectToChrome(ginkgoT GinkgoTInterface, options ...BilobaConfigOption) *Biloba {
	ginkgoT.Helper()
	b := newBiloba(ginkgoT)
	b.root = b

	for _, option := range options {
		option(b)
	}

	if b.ChromeConnection.WebSocketURL == "" {
		var cc ChromeConnection
		configFilePath := gooseConfigPath(ginkgoT.ParallelProcess())
		if _, err := os.Stat(configFilePath); err != nil {
			configFilePath = gooseConfigPath(1)
		}
		data, err := os.ReadFile(configFilePath)
		if err != nil {
			ginkgoT.Fatalf("failed to load ChromeConnection: %w", err)
			return nil
		}
		err = json.Unmarshal(data, &cc)
		if err != nil {
			ginkgoT.Fatalf("failed to decode ChromeConnection: %w", err)
			return nil
		}
		b.ChromeConnection = cc
	}
	allocatorContext, cancel := chromedp.NewRemoteAllocator(context.Background(), b.ChromeConnection.WebSocketURL)
	b.gt.DeferCleanup(cancel)

	cOptions := []chromedp.ContextOption{chromedp.WithNewBrowserContext()}
	if b.enableDebugLogging {
		cOptions = append(cOptions, chromedp.WithDebugf(b.gt.Logf))
		cOptions = append(cOptions, chromedp.WithLogf(b.gt.Logf))
		cOptions = append(cOptions, chromedp.WithErrorf(b.gt.Logf))
	}

	b.Context, cancel = chromedp.NewContext(allocatorContext, cOptions...)
	b.gt.DeferCleanup(cancel)
	_, err := b.RunErr("1")

	b.targetID = chromedp.FromContext(b.Context).Target.TargetID
	b.browserContextID = chromedp.FromContext(b.Context).BrowserContextID

	b.downloadDir = b.gt.TempDir()
	b.setUpListeners()

	if err != nil {
		ginkgoT.Fatalf("failed to connect to chrome: %w", err)
		return nil
	}

	b.lock.Lock()
	b.tabs[chromedp.FromContext(b.Context).Target.TargetID] = b
	b.lock.Unlock()

	return b
}

type Biloba struct {
	Context          context.Context
	gt               GinkgoTInterface
	ChromeConnection ChromeConnection

	targetID         target.ID
	browserContextID cdp.BrowserContextID

	lock  *sync.Mutex
	root  *Biloba
	tabs  map[target.ID]*Biloba
	close context.CancelFunc

	bilobaIsInstalled bool

	downloadDir     string
	downloads       map[string]*Download
	downloadHistory map[string]time.Time

	dialogHandlers []*DialogHandler
	dialogs        []*Dialog

	enableDebugLogging               bool
	disableFailureScreenshots        bool
	disableProgressReportScreenshots bool
}

func newBiloba(ginkgoT GinkgoTInterface) *Biloba {
	b := &Biloba{
		gt:              ginkgoT,
		lock:            &sync.Mutex{},
		downloads:       map[string]*Download{},
		downloadHistory: map[string]time.Time{},
		tabs:            map[target.ID]*Biloba{},
	}
	return b
}

func (b *Biloba) BrowserContextID() cdp.BrowserContextID {
	return b.browserContextID
}

func (b *Biloba) Description() string {
	return fmt.Sprintf("%s - Target-ID: %s | Browser-ID: %s ", b.Title(), b.targetID, b.browserContextID)
}

func (b *Biloba) Prepare() {
	if !b.isRootTab() {
		return
	}
	//close all tabs
	closedTabs := false
	for _, tab := range b.AllTabs() {
		if !tab.isRootTab() {
			b.root.lock.Lock()
			delete(b.root.tabs, chromedp.FromContext(tab.Context).Target.TargetID)
			b.root.lock.Unlock()
			tab.close()
			closedTabs = true
		}
	}
	//closing all those tabs means we may have nuked our download config, so we reset it
	if closedTabs {
		b.configureDownloadBehavior()
	}

	b.lock.Lock()
	b.downloads = map[string]*Download{}
	b.dialogHandlers = []*DialogHandler{}
	b.dialogs = Dialogs{}
	b.lock.Unlock()

	if !b.disableFailureScreenshots {
		b.gt.DeferCleanup(b.AttachScreenshotsIfFailed)
	}
	if !b.disableProgressReportScreenshots {
		b.gt.DeferCleanup(b.gt.AttachProgressReporter(b.progressReporter))
	}

	b.Navigate("about:blank")
}

func (b *Biloba) NewTab() *Biloba {
	return b.registerTabFor(chromedp.NewContext(b.root.Context, chromedp.WithNewBrowserContext()))
}

func (b *Biloba) AllTabs() []*Biloba {
	targets, err := chromedp.Targets(b.root.Context)
	if err != nil {
		b.gt.Fatalf("Failed to list tabs:\n%s", err.Error())
	}
	tabs := []*Biloba{}

	for _, target := range targets {
		b.root.lock.Lock()
		tab, ok := b.root.tabs[target.TargetID]
		b.root.lock.Unlock()
		if !ok {
			// this may be a new tab we've never seen before - is it ours?
			opener := b.root.tabs[target.OpenerID]
			if opener != nil {
				tab = b.root.registerTabFor(chromedp.NewContext(opener.Context, chromedp.WithTargetID(target.TargetID)))
			} else {
				continue
			}
		}
		tabs = append(tabs, tab)
	}
	return tabs
}

func (b *Biloba) isRootTab() bool {
	return b.root == b
}

/*
also test things to validate that we do, in fact, need to do this (i.e. specifically - is it true that a tab that has the same browser context as other tabs will nuke those tabs' download configuration if it is closed?)

this will entail renaming these helper functions to (e.b.) configureDownloadBehaviorForAllTabsWithBrowserContextID(...)
*/
func (b *Biloba) Close() error {
	if b.isRootTab() {
		return fmt.Errorf("invalid attempt to close the root tab")
	}

	/*
		any tabs that share this tab's BrowserContextID will fail to download things when this tab is closed that is because we need to configure chrome's download behavior on each tab in order to be able to catch downloads however closing just one tab causes chrome to clear out that download behavior

		so...

		#1 we error if an active download is in place - users must Eventually(b.CloseTab).Should(Succeed())`
	*/
	if b.root.activeDownloadsShouldBlockTabFromClosing(b) {
		return fmt.Errorf("cannot close tab because another tab is actively downloading a file and closing this tab would cause that download to fail, please try again later")
	}
	b.root.lock.Lock()
	delete(b.root.tabs, chromedp.FromContext(b.Context).Target.TargetID)
	b.root.lock.Unlock()
	b.close()
	/*
		#2 we must reconfigure the download behavior for all tabs with this tab's browserContextID once this tab is closed
	*/
	b.root.configureDownloadBehaviorForAllTabsWithBrowserContextID(b.browserContextID)
	return nil
}

func (b *Biloba) AttachScreenshotsIfFailed() {
	if b.gt.Failed() {
		for _, screenshot := range b.safeAllTabScreenshots() {
			if screenshot.failure != "" {
				b.gt.AddReportEntryVisibilityFailureOrVerbose(screenshot.failure)
			} else {
				b.gt.AddReportEntryVisibilityFailureOrVerbose(fmt.Sprintf("Screenshot for: '%s'", screenshot.title), screenshot.imgcatScreenshot)
			}
		}
	}
}

func (b *Biloba) progressReporter() string {
	out := ""
	for _, screenshot := range b.safeAllTabScreenshots() {
		if screenshot.failure != "" {
			out += b.gt.F("{{red}}" + screenshot.failure + "{{/}}\n")
		} else {
			out += b.gt.F("{{bold}}Screenshot for: '%s'{{/}}\n", screenshot.title)
			out += screenshot.imgcatScreenshot + "\n"
		}
	}
	return out
}

func (b *Biloba) registerTabFor(c context.Context, cancel context.CancelFunc) *Biloba {
	b.gt.Helper()
	newG := newBiloba(b.gt)
	newG.Context = c
	newG.ChromeConnection = b.ChromeConnection
	newG.downloadDir = b.downloadDir
	newG.root = b.root
	newG.close = cancel

	//spin up the tab
	newG.Run("1")
	newG.targetID = chromedp.FromContext(newG.Context).Target.TargetID

	var browserContextID cdp.BrowserContextID
	err := chromedp.Run(c,
		chromedp.ActionFunc(func(ctx context.Context) error {
			info, err := target.GetTargetInfo().Do(ctx)
			browserContextID = info.BrowserContextID
			return err
		}),
	)
	if err != nil {
		b.gt.Fatalf("Failed to register new tab: %s", err.Error())
	}

	newG.browserContextID = browserContextID
	newG.setUpListeners()

	b.root.lock.Lock()
	b.root.tabs[newG.targetID] = newG
	b.root.lock.Unlock()

	return newG
}

//go:embed biloba.js
var bilobaJS string

func (b *Biloba) handleEventFrameNavigated(ev *page.EventFrameNavigated) {
	b.lock.Lock()
	defer b.lock.Unlock()
	b.bilobaIsInstalled = false
}

func (b *Biloba) ensureBiloba() {
	b.lock.Lock()
	installed := b.bilobaIsInstalled
	b.lock.Unlock()
	if installed {
		return
	}
	b.Run(bilobaJS)
	b.lock.Lock()
	b.bilobaIsInstalled = true
	b.lock.Unlock()
}
