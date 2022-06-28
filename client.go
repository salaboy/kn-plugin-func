package function

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"time"

	"github.com/mitchellh/go-homedir"
)

const (
	// DefaultRegistry through which containers of Functions will be shuttled.
	DefaultRegistry = "docker.io"

	// DefaultTemplate is the default Function signature / environmental context
	// of the resultant function.  All runtimes are expected to have at least
	// one implementation of each supported function signature.  Currently that
	// includes an HTTP Handler ("http") and Cloud Events handler ("events")
	DefaultTemplate = "http"

	// DefaultVersion is the initial value for string members whose implicit type
	// is a semver.
	DefaultVersion = "0.0.0"

	// DefaultConfigPath is used in the unlikely event that
	// the user has no home directory (no ~), there is no
	// XDG_CONFIG_HOME set, and no WithConfigPath was used.
	DefaultConfigPath = ".config/func"

	// DefaultBuildType is the default build type for a Function
	DefaultBuildType = BuildTypeLocal

	// RunDataDir holds transient runtime metadata
	// By default it is excluded from source control.
	RunDataDir = ".func"

	// buildstamp is the name of the file within the run data directory whose
	// existence indicates the Function has been built, and whose content is
	// a fingerprint of the filesystem at the time of the build.
	buildstamp = "built"
)

// Client for managing Function instances.
type Client struct {
	repositoriesPath  string            // path to repositories
	repositoriesURI   string            // repo URI (overrides repositories path)
	verbose           bool              // print verbose logs
	builder           Builder           // Builds a runnable image source
	pusher            Pusher            // Pushes Funcation image to a remote
	deployer          Deployer          // Deploys or Updates a Function
	runner            Runner            // Runs the Function locally
	remover           Remover           // Removes remote services
	lister            Lister            // Lists remote services
	describer         Describer         // Describes Function instances
	dnsProvider       DNSProvider       // Provider of DNS services
	registry          string            // default registry for OCI image tags
	progressListener  ProgressListener  // progress listener
	repositories      *Repositories     // Repositories management
	templates         *Templates        // Templates management
	instances         *Instances        // Function Instances management
	transport         http.RoundTripper // Customizable internal transport
	pipelinesProvider PipelinesProvider // CI/CD pipelines management
}

// ErrNotBuilt indicates the Function has not yet been built.
var ErrNotBuilt = errors.New("not built")

// Builder of Function source to runnable image.
type Builder interface {
	// Build a Function project with source located at path.
	Build(context.Context, Function) error
}

// Pusher of Function image to a registry.
type Pusher interface {
	// Push the image of the Function.
	// Returns Image Digest - SHA256 hash of the produced image
	Push(ctx context.Context, f Function) (string, error)
}

// Deployer of Function source to running status.
type Deployer interface {
	// Deploy a Function of given name, using given backing image.
	Deploy(context.Context, Function) (DeploymentResult, error)
}

type DeploymentResult struct {
	Status Status
	URL    string
}

// Status of the Function from the DeploymentResult
type Status int

const (
	Failed Status = iota
	Deployed
	Updated
)

// Runner runs the Function locally.
type Runner interface {
	// Run the Function, returning a Job with metadata, error channels, and
	// a stop function.The process can be stopped by running the returned stop
	// function, either on context cancellation or in a defer.
	Run(context.Context, Function) (*Job, error)
}

// Remover of deployed services.
type Remover interface {
	// Remove the Function from remote.
	Remove(ctx context.Context, name string) error
}

// Lister of deployed functions.
type Lister interface {
	// List the Functions currently deployed.
	List(ctx context.Context) ([]ListItem, error)
}

type ListItem struct {
	Name      string `json:"name" yaml:"name"`
	Namespace string `json:"namespace" yaml:"namespace"`
	Runtime   string `json:"runtime" yaml:"runtime"`
	URL       string `json:"url" yaml:"url"`
	Ready     string `json:"ready" yaml:"ready"`
}

// ProgressListener is notified of task progress.
type ProgressListener interface {
	// SetTotal steps of the given task.
	SetTotal(int)
	// Increment to the next step with the given message.
	Increment(message string)
	// Complete signals completion, which is expected to be somewhat different
	// than a step increment.
	Complete(message string)
	// Stopping indicates the process is in the state of stopping, such as when a
	// context cancelation has been received
	Stopping()
	// Done signals a cessation of progress updates.  Should be called in a defer
	// statement to ensure the progress listener can stop any outstanding tasks
	// such as synchronous user updates.
	Done()
}

// Describer of Function instances
type Describer interface {
	// Describe the named Function in the remote environment.
	Describe(ctx context.Context, name string) (Instance, error)
}

// Instance data about the runtime state of a Function in a given environment.
//
// A Function instance is a logical running Function space, which share
// a unique route (or set of routes).  Due to autoscaling and load balancing,
// there is a one to many relationship between a given route and processes.
// By default the system creates the 'local' and 'remote' named instances
// when a Function is run (locally) and deployed, respectively.
// See the .Instances(f) accessor for the map of named environments to these
// Function Information structures.
type Instance struct {
	// Route is the primary route of a Function instance.
	Route string
	// Routes is the primary route plus any other route at which the Function
	// can be contacted.
	Routes        []string       `json:"routes" yaml:"routes"`
	Name          string         `json:"name" yaml:"name"`
	Image         string         `json:"image" yaml:"image"`
	Namespace     string         `json:"namespace" yaml:"namespace"`
	Subscriptions []Subscription `json:"subscriptions" yaml:"subscriptions"`
}

// Subscriptions currently active to event sources
type Subscription struct {
	Source string `json:"source" yaml:"source"`
	Type   string `json:"type" yaml:"type"`
	Broker string `json:"broker" yaml:"broker"`
}

// DNSProvider exposes DNS services necessary for serving the Function.
type DNSProvider interface {
	// Provide the given name by routing requests to address.
	Provide(Function) error
}

// PipelinesProvider manages lifecyle of CI/CD pipelines used by a Function
type PipelinesProvider interface {
	Run(context.Context, Function) error
	Remove(context.Context, Function) error
}

// New client for Function management.
func New(options ...Option) *Client {
	// Instantiate client with static defaults.
	c := &Client{
		builder:           &noopBuilder{output: os.Stdout},
		pusher:            &noopPusher{output: os.Stdout},
		deployer:          &noopDeployer{output: os.Stdout},
		runner:            &noopRunner{output: os.Stdout},
		remover:           &noopRemover{output: os.Stdout},
		lister:            &noopLister{output: os.Stdout},
		describer:         &noopDescriber{output: os.Stdout},
		dnsProvider:       &noopDNSProvider{output: os.Stdout},
		progressListener:  &NoopProgressListener{},
		pipelinesProvider: &noopPipelinesProvider{},
		repositoriesPath:  filepath.Join(ConfigPath(), "repositories"),
		transport:         http.DefaultTransport,
	}
	for _, o := range options {
		o(c)
	}

	// Initialize sub-managers using now-fully-initialized client.
	c.repositories = newRepositories(c)
	c.templates = newTemplates(c)
	c.instances = newInstances(c)

	// Trigger the creation of the config and repository paths
	_ = ConfigPath()         // Config is package-global scoped
	_ = c.RepositoriesPath() // Repositories is Client-specific

	return c
}

// The default config path is evaluated in the following order, from lowest
// to highest precedence.
// 1.  The static default is DefaultConfigPath (./.config/func)
// 2.  ~/.config/func if it exists (can be expanded: user has a home dir)
// 3.  The value of $XDG_CONFIG_PATH/func if the environment variable exists.
// The path will be created if it does not already exist.
func ConfigPath() (path string) {
	path = DefaultConfigPath

	// ~/.config/func is the default if ~ can be expanded
	if home, err := homedir.Expand("~"); err == nil {
		path = filepath.Join(home, ".config", "func")
	}

	// 'XDG_CONFIG_HOME/func' takes precidence if defined
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		path = filepath.Join(xdg, "func")
	}

	mkdir(path) // make sure it exists
	return
}

// RepositoriesPath accesses the currently effective repositories path,
// which defaults to [ConfigPath]/repositories but can be set explicitly using
// the WithRepositoriesPath option when creating the client..
// The path will be created if it does not already exist.
func (c *Client) RepositoriesPath() (path string) {
	path = c.repositories.Path()
	mkdir(path) // make sure it exists
	return
}

// RepositoriesPath is a convenience method for accessing the default path to
// repositories that will be used by new instances of a Client unless options
// such as WithRepositoriesPath are used to override.
// The path will be created if it does not already exist.
func RepositoriesPath() string {
	return New().RepositoriesPath()
}

// OPTIONS
// ---------

// Option defines a Function which when passed to the Client constructor
// optionally mutates private members at time of instantiation.
type Option func(*Client)

// WithVerbose toggles verbose logging.
func WithVerbose(v bool) Option {
	return func(c *Client) {
		c.verbose = v
	}
}

// WithBuilder provides the concrete implementation of a builder.
func WithBuilder(d Builder) Option {
	return func(c *Client) {
		c.builder = d
	}
}

// WithPusher provides the concrete implementation of a pusher.
func WithPusher(d Pusher) Option {
	return func(c *Client) {
		c.pusher = d
	}
}

// WithDeployer provides the concrete implementation of a deployer.
func WithDeployer(d Deployer) Option {
	return func(c *Client) {
		c.deployer = d
	}
}

// WithRunner provides the concrete implementation of a deployer.
func WithRunner(r Runner) Option {
	return func(c *Client) {
		c.runner = r
	}
}

// WithRemover provides the concrete implementation of a remover.
func WithRemover(r Remover) Option {
	return func(c *Client) {
		c.remover = r
	}
}

// WithLister provides the concrete implementation of a lister.
func WithLister(l Lister) Option {
	return func(c *Client) {
		c.lister = l
	}
}

// WithDescriber provides a concrete implementation of a Function describer.
func WithDescriber(describer Describer) Option {
	return func(c *Client) {
		c.describer = describer
	}
}

// WithProgressListener provides a concrete implementation of a listener to
// be notified of progress updates.
func WithProgressListener(p ProgressListener) Option {
	return func(c *Client) {
		c.progressListener = p
	}
}

// WithDNSProvider proivdes a DNS provider implementation for registering the
// effective DNS name which is either explicitly set via WithName or is derived
// from the root path.
func WithDNSProvider(provider DNSProvider) Option {
	return func(c *Client) {
		c.dnsProvider = provider
	}
}

// WithRepositoriesPath sets the location on disk to use for extensible template
// repositories.  Extensible template repositories are additional templates
// that exist on disk and are not built into the binary.
func WithRepositoriesPath(path string) Option {
	return func(c *Client) {
		c.repositoriesPath = path
	}
}

// WithRepository sets a specific URL to a Git repository from which to pull
// templates.  This setting's existence precldes the use of either the inbuilt
// templates or any repositories from the extensible repositories path.
func WithRepository(uri string) Option {
	return func(c *Client) {
		c.repositoriesURI = uri
	}
}

// WithRegistry sets the default registry which is consulted when an image name/tag
// is not explocitly provided.  Can be fully qualified, including the registry
// (ex: 'quay.io/myname') or simply the namespace 'myname' which indicates the
// the use of the default registry.
func WithRegistry(registry string) Option {
	return func(c *Client) {
		c.registry = registry
	}
}

// WithTransport sets a custom transport to use internally.
func WithTransport(t http.RoundTripper) Option {
	return func(c *Client) {
		c.transport = t
	}
}

// WithPipelinesProvider sets implementation of provider responsible for CI/CD pipelines
func WithPipelinesProvider(pp PipelinesProvider) Option {
	return func(c *Client) {
		c.pipelinesProvider = pp
	}
}

// ACCESSORS
// ---------

// Repositories accessor
func (c *Client) Repositories() *Repositories {
	return c.repositories
}

// Templates accessor
func (c *Client) Templates() *Templates {
	return c.templates
}

// Instances accessor
func (c *Client) Instances() *Instances {
	return c.instances
}

// Runtimes available in totality.
// Not all repository/template combinations necessarily exist,
// and further validation is performed when a template+runtime is chosen.
// from a given repository.  This is the global list of all available.
// Returned list is unique and sorted.
func (c *Client) Runtimes() ([]string, error) {
	runtimes := newSortedSet()

	// Gather all runtimes from all repositories
	// into a uniqueness map
	repositories, err := c.Repositories().All()
	if err != nil {
		return []string{}, err
	}
	for _, repo := range repositories {
		for _, runtime := range repo.Runtimes {
			runtimes.Add(runtime.Name)
		}
	}

	// Return a unique, sorted list of runtimes
	return runtimes.Items(), nil
}

// LIFECYCLE METHODS
// -----------------

// New Function.
// Use Create, Build and Deploy independently for lower level control.
func (c *Client) New(ctx context.Context, cfg Function) (err error) {
	c.progressListener.SetTotal(3)
	// Always start a concurrent routine listening for context cancellation.
	// On this event, immediately indicate the task is canceling.
	// (this is useful, for example, when a progress listener is mutating
	// stdout, and a context cancelation needs to free up stdout entirely for
	// the status or error from said cancelltion.
	go func() {
		<-ctx.Done()
		c.progressListener.Stopping()
	}()

	// Create Function at path indidcated by Config
	if err = c.Create(cfg); err != nil {
		return
	}

	// Load the now-initialized Function.
	f, err := NewFunction(cfg.Root)
	if err != nil {
		return
	}

	// Build the now-initialized Function
	c.progressListener.Increment("Building container image")
	if err = c.Build(ctx, f.Root); err != nil {
		return
	}

	// Push the produced function image
	c.progressListener.Increment("Pushing container image to registry")
	if err = c.Push(ctx, f.Root); err != nil {
		return
	}

	// Deploy the initialized Function, returning its publicly
	// addressible name for possible registration.
	c.progressListener.Increment("Deploying Function to cluster")
	if err = c.Deploy(ctx, f.Root); err != nil {
		return
	}

	// Create an external route to the Function
	c.progressListener.Increment("Creating route to Function")
	if err = c.Route(f.Root); err != nil {
		return
	}

	c.progressListener.Complete("Done")

	// TODO: use the knative client during deployment such that the actual final
	// route can be returned from the deployment step, passed to the DNS Router
	// for routing actual traffic, and returned here.
	if c.verbose {
		fmt.Printf("https://%v/\n", f.Name)
	}
	return
}

// Create a new Function from the given defaults.
// <path> will default to the absolute path of the current working directory.
// <name> will default to the current working directory.
// When <name> is provided but <path> is not, a directory <name> is created
// in the current working directory and used for <path>.
func (c *Client) Create(cfg Function) (err error) {
	// convert Root path to absolute
	cfg.Root, err = filepath.Abs(cfg.Root)
	if err != nil {
		return
	}

	// Create project root directory, if it doesn't already exist
	if err = os.MkdirAll(cfg.Root, 0755); err != nil {
		return
	}

	// Create should never clobber a pre-existing Function
	hasFunc, err := hasInitializedFunction(cfg.Root)
	if err != nil {
		return err
	}
	if hasFunc {
		return fmt.Errorf("Function at '%v' already initialized", cfg.Root)
	}

	// Path is defaulted to the current working directory
	if cfg.Root == "" {
		if cfg.Root, err = os.Getwd(); err != nil {
			return
		}
	}

	// Name is defaulted to the directory of the given path.
	if cfg.Name == "" {
		cfg.Name = nameFromPath(cfg.Root)
	}

	// The path for the new Function should not have any contentious files
	// (hidden files OK, unless it's one used by Func)
	if err := assertEmptyRoot(cfg.Root); err != nil {
		return err
	}

	// Create a new Function (in memory)
	f := NewFunctionWith(cfg)

	// Create a .func diretory which is also added to a .gitignore
	if err = ensureRuntimeDir(f); err != nil {
		return
	}

	// Write out the new Function's Template files.
	// Templates contain values which may result in the Function being mutated
	// (default builders, etc), so a new (potentially mutated) Function is
	// returned from Templates.Write
	err = c.Templates().Write(&f)
	if err != nil {
		return
	}

	// Mark the Function as having been created
	f.Created = time.Now()
	if err = f.Write(); err != nil {
		return
	}

	return
}

// Tag the Function as having been built
// This is locally-scoped data, only indicating there presumably exists
// a container image in the cache of the the configured builder, thus this info
// is placed in a .func (non-source controlled) local metadata directory, which
// is not stritly required to exist, so it is created if needed.
func updateBuildStamp(f Function) (err error) {
	if err = ensureRuntimeDir(f); err != nil {
		return err
	}
	hash, err := fingerprint(f)
	if err != nil {
		return err
	}
	if err = os.WriteFile(filepath.Join(f.Root, RunDataDir, buildstamp), []byte(hash), os.ModePerm); err != nil {
		return err
	}
	return
}

// ensureRuntimeDir creates a .func directory in the root of the given
// Function which is also registered as ignored in .gitignore
// TODO: Mutate extant .gitignore file if it exists rather than failing
// if present (see contentious files in function.go), such that a user
// can `git init` a directory prior to `func init` in the same directory).
func ensureRuntimeDir(f Function) error {
	if err := os.MkdirAll(filepath.Join(f.Root, RunDataDir), os.ModePerm); err != nil {
		return err
	}

	gitignore := `
# Functions use the .func directory for local runtime data which should
# generally not be tracked in source control:
/.func
`
	return os.WriteFile(filepath.Join(f.Root, ".gitignore"), []byte(gitignore), os.ModePerm)

}

// Build the Function at path. Errors if the Function is either unloadable or does
// not contain a populated Image.
func (c *Client) Build(ctx context.Context, path string) (err error) {
	c.progressListener.Increment("Building function image")

	// If not logging verbosely, the ongoing progress of the build will not
	// be streaming to stdout, and the lack of activity has been seen to cause
	// users to prematurely exit due to the sluggishness of pulling large images
	if !c.verbose {
		c.printBuildActivity(ctx) // print friendly messages until context is canceled
	}

	f, err := NewFunction(path)
	if err != nil {
		return
	}

	// Derive Image from the path (precedence is given to extant config)
	if f.Runtime.Image, err = DerivedImage(path, c.registry); err != nil {
		return
	}

	if err = c.builder.Build(ctx, f); err != nil {
		return
	}

	// Write (save) - Serialize the Function to disk
	// Will now contain populated image tag.
	if err = f.Write(); err != nil {
		return
	}

	// Tag the function as having been built
	if err = updateBuildStamp(f); err != nil {
		return
	}

	// TODO: create a status structure and return it here for optional
	// use by the cli for user echo (rather than rely on verbose mode here)
	message := fmt.Sprintf("🙌 Function image built: %v", f.Runtime.Image)
	if runtime.GOOS == "windows" {
		message = fmt.Sprintf("Function image built: %v", f.Runtime.Image)
	}
	c.progressListener.Increment(message)
	return
}

func (c *Client) printBuildActivity(ctx context.Context) {
	m := []string{
		"Still building",
		"Still building",
		"Yes, still building",
		"Don't give up on me",
		"Still building",
		"This is taking a while",
	}
	i := 0
	ticker := time.NewTicker(10 * time.Second)
	go func() {
		for {
			select {
			case <-ticker.C:
				c.progressListener.Increment(m[i])
				i++
				i = i % len(m)
			case <-ctx.Done():
				c.progressListener.Stopping()
				ticker.Stop()
				return
			}
		}
	}()
}

// Deploy the Function at path. Errors if the Function has not been
// initialized with an image tag.
func (c *Client) Deploy(ctx context.Context, path string) (err error) {
	go func() {
		<-ctx.Done()
		c.progressListener.Stopping()
	}()

	f, err := NewFunction(path)
	if err != nil {
		return
	}

	// Functions must be built (have an associated image) before being deployed.
	// Note that externally built images may be specified in the func.yaml
	if !f.HasImage() {
		return ErrNotBuilt
	}

	// Deploy a new or Update the previously-deployed Function
	c.progressListener.Increment("Deploying function to the cluster")
	result, err := c.deployer.Deploy(ctx, f)
	if result.Status == Deployed {
		c.progressListener.Increment(fmt.Sprintf("Function deployed at URL: %v", result.URL))
	} else if result.Status == Updated {
		c.progressListener.Increment(fmt.Sprintf("Function updated at URL: %v", result.URL))
	}

	return err
}

// RunPipeline runs a Pipeline to Build and deploy the Function at path.
func (c *Client) RunPipeline(ctx context.Context, path string, git Git) (err error) {
	go func() {
		<-ctx.Done()
		c.progressListener.Stopping()
	}()

	f, err := NewFunction(path)
	if err != nil {
		err = fmt.Errorf("failed to laod function: %w", err)
		return
	}
	f.Build.Git = git

	// Build and deploy function using Pipeline
	err = c.pipelinesProvider.Run(ctx, f)
	if err != nil {
		err = fmt.Errorf("failed to run pipeline: %w", err)
		return
	}

	return err
}

func (c *Client) Route(path string) (err error) {
	// Ensure that the allocated final address is enabled with the
	// configured DNS provider.
	// NOTE:
	// DNS and TLS are provisioned by Knative Serving + cert-manager,
	// but DNS subdomain CNAME to the Kourier Load Balancer is
	// still manual, and the initial cluster config to suppot the TLD
	// is still manual.
	f, err := NewFunction(path)
	if err != nil {
		return
	}
	return c.dnsProvider.Provide(f)
}

// Run the Function whose code resides at root.
// On start, the chosen port is sent to the provided started channel
func (c *Client) Run(ctx context.Context, root string) (job *Job, err error) {
	go func() {
		<-ctx.Done()
		c.progressListener.Stopping()
	}()

	// Load the Function
	f, err := NewFunction(root)
	if err != nil {
		return
	}
	if !f.Initialized() {
		// TODO: this needs a test.
		err = fmt.Errorf("the given path '%v' does not contain an initialized "+
			"Function.  Please create one at this path in order to run", root)
		return
	}

	// Run the Function, which returns a Job for use interacting (at arms length)
	// with that running task (which is likely inside a container process).
	if job, err = c.runner.Run(ctx, f); err != nil {
		return
	}

	// Return to the caller the effective port, a function to call to trigger
	// stop, and a channel on which can be received runtime errors.
	return job, nil
}

// Info for a Function.  Name takes precidence.  If no name is provided,
// the Function defined at root is used.
func (c *Client) Info(ctx context.Context, name, root string) (d Instance, err error) {
	go func() {
		<-ctx.Done()
		c.progressListener.Stopping()
	}()
	// If name is provided, it takes precidence.
	// Otherwise load the Function defined at root.
	if name != "" {
		return c.describer.Describe(ctx, name)
	}

	f, err := NewFunction(root)
	if err != nil {
		return d, err
	}
	if !f.Initialized() {
		return d, fmt.Errorf("%v is not initialized", f.Name)
	}
	return c.describer.Describe(ctx, f.Name)
}

// List currently deployed Functions.
func (c *Client) List(ctx context.Context) ([]ListItem, error) {
	// delegate to concrete implementation of lister entirely.
	return c.lister.List(ctx)
}

// Remove a Function.  Name takes precidence.  If no name is provided,
// the Function defined at root is used if it exists.
func (c *Client) Remove(ctx context.Context, cfg Function, deleteAll bool) error {
	go func() {
		<-ctx.Done()
		c.progressListener.Stopping()
	}()
	// If name is provided, it takes precidence.
	// Otherwise load the Function defined at root.
	functionName := cfg.Name
	if cfg.Name == "" {
		f, err := NewFunction(cfg.Root)
		if err != nil {
			return err
		}
		if !f.Initialized() {
			return fmt.Errorf("Function at %v can not be removed unless initialized. Try removing by name", f.Root)
		}
		functionName = f.Name
		cfg = f
	}

	// Delete Knative Service and dependent resources in parallel
	c.progressListener.Increment(fmt.Sprintf("Removing Knative Service: %v", functionName))
	errChan := make(chan error)
	go func() {
		errChan <- c.remover.Remove(ctx, functionName)
	}()

	var errResources error
	if deleteAll {
		c.progressListener.Increment(fmt.Sprintf("Removing Knative Service '%v' and all dependent resources", functionName))
		errResources = c.pipelinesProvider.Remove(ctx, cfg)
	}

	errService := <-errChan

	if errService != nil && errResources != nil {
		return fmt.Errorf("%s\n%s", errService, errResources)
	} else if errResources != nil {
		return errResources
	}
	return errService
}

// Invoke is a convenience method for triggering the execution of a Function
// for testing and development.  Returned is a map of metadata and a stringified
// version of the content.
// The target argument is optional, naming the running instance of the Function
// which should be invoked.  This can be the literal names "local" or "remote",
// or can be a URL to an arbitrary endpoint.  If not provided, a running local
// instance is preferred, with the remote Function triggered if there is no
// locally running instance.
// Example:
//  myClient.Invoke(myContext, myFunction, "local", NewInvokeMessage())
// The message sent to the Function is defined by the invoke message.
// See NewInvokeMessage for its defaults.
// Functions are invoked in a manner consistent with the settings defined in
// their metadata.  For example HTTP vs CloudEvent
func (c *Client) Invoke(ctx context.Context, root string, target string, m InvokeMessage) (metadata map[string][]string, body string, err error) {
	go func() {
		<-ctx.Done()
		c.progressListener.Stopping()
	}()

	f, err := NewFunction(root)
	if err != nil {
		return
	}

	// See invoke.go for implementation details
	return invoke(ctx, c, f, target, m, c.verbose)
}

// Push the image for the named service to the configured registry
func (c *Client) Push(ctx context.Context, path string) (err error) {
	f, err := NewFunction(path)
	if err != nil {
		return
	}

	if !f.HasImage() {
		return ErrNotBuilt
	}

	imageDigest, err := c.pusher.Push(ctx, f)
	if err != nil {
		return
	}

	// Record the Image Digest pushed.
	f.Runtime.ImageDigest = imageDigest
	return f.Write()
}

// Built returns true if the given path contains a Function which has been
// built without any filesystem modifications since (is not stale).
func (c *Client) Built(path string) bool {
	f, err := NewFunction(path)
	if err != nil {
		return false
	}

	// Missing a build image always means !Built (but does not satisfy staleness
	// checks).
	// NOTE: This will be updated in the future such that a build does not
	// automatically update the Function's serialized, source-controlled state,
	// because merely building does not indicate the Function has changed, but
	// rather that field should be populated on deploy.  I.e. the Image name
	// and image stamp should reside as transient data in .func until such time
	// as the given image has been deployed.
	// An example of how this bug manifests is that every rebuild of a Function
	// registers the func.yaml as being dirty for source-control purposes, when
	// this should only happen on deploy.
	if !f.HasImage() {
		return false
	}

	buildstampPath := filepath.Join(path, RunDataDir, buildstamp)

	// If there is no build stamp, it is also not built.
	// This case should be redundant with the above check for an image, but is
	// temporarily necessary (see the long-winded caviat note above).
	if _, err := os.Stat(buildstampPath); err != nil {
		return false
	}

	// Calculate the Function's Filesystem hash and see if it has changed.
	hash, err := fingerprint(f)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error calculating Function's fingerprint: %v\n", err)
		return false
	}
	b, err := os.ReadFile(buildstampPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error reading Function's fingerprint: %v\n", err)
		return false
	}
	if string(b) != hash {
		return false // changes detected
	}

	// Function has a populated image, existing buildstamp, and the calculated
	// fingerprint has not changed.
	// It's a pretty good chance the thing doesn't need to be rebuilt, though
	// of course filesystem racing conditions do exist, including both direct
	// source code modifications or changes to the image cache.
	return true
}

// fingerprint returns a hash of the filenames and modification timestamps of
// the files within a Function's root.
func fingerprint(f Function) (string, error) {
	h := sha256.New()
	err := filepath.Walk(f.Root, func(path string, info fs.FileInfo, err error) error {
		if err != nil {
			return err
		}
		// Always ignore .func, .git (TODO: .funcignore)
		if info.IsDir() && (info.Name() == RunDataDir || info.Name() == ".git") {
			return filepath.SkipDir
		}
		fmt.Fprintf(h, "%v:%v:", path, info.ModTime().UnixNano())
		return nil
	})
	return fmt.Sprintf("%x", h.Sum(nil)), err
}

// DEFAULTS
// ---------

// Manual implementations (noops) of required interfaces.
// In practice, the user of this client package (for example the CLI) will
// provide a concrete implementation for only the interfaces necessary to
// complete the given command.  Integrators importing the package would
// provide a concrete implementation for all interfaces to be used. To
// enable partial definition (in particular used for testing) they
// are defaulted to noop implementations such that they can be provded
// only when necessary.  Unit tests for the concrete implementations
// serve to keep the core logic here separate from the imperitive, and
// with a minimum of external dependencies.
// -----------------------------------------------------

// Builder
type noopBuilder struct{ output io.Writer }

func (n *noopBuilder) Build(ctx context.Context, _ Function) error { return nil }

// Pusher
type noopPusher struct{ output io.Writer }

func (n *noopPusher) Push(ctx context.Context, f Function) (string, error) { return "", nil }

// Deployer
type noopDeployer struct{ output io.Writer }

func (n *noopDeployer) Deploy(ctx context.Context, _ Function) (DeploymentResult, error) {
	return DeploymentResult{}, nil
}

// Runner
type noopRunner struct{ output io.Writer }

func (n *noopRunner) Run(context.Context, Function) (job *Job, err error) {
	return
}

// Remover
type noopRemover struct{ output io.Writer }

func (n *noopRemover) Remove(context.Context, string) error { return nil }

// Lister
type noopLister struct{ output io.Writer }

func (n *noopLister) List(context.Context) ([]ListItem, error) { return []ListItem{}, nil }

// Describer
type noopDescriber struct{ output io.Writer }

func (n *noopDescriber) Describe(context.Context, string) (Instance, error) {
	return Instance{}, errors.New("no describer provided")
}

// PipelinesProvider
type noopPipelinesProvider struct{}

func (n *noopPipelinesProvider) Run(ctx context.Context, _ Function) error    { return nil }
func (n *noopPipelinesProvider) Remove(ctx context.Context, _ Function) error { return nil }

// DNSProvider
type noopDNSProvider struct{ output io.Writer }

func (n *noopDNSProvider) Provide(_ Function) error { return nil }

// ProgressListener
type NoopProgressListener struct{}

func (p *NoopProgressListener) SetTotal(i int)     {}
func (p *NoopProgressListener) Increment(m string) {}
func (p *NoopProgressListener) Complete(m string)  {}
func (p *NoopProgressListener) Stopping()          {}
func (p *NoopProgressListener) Done()              {}

// mkdir attempts to mkdir, writing any errors to stderr.
func mkdir(path string) {
	// Since it is expected that the code elsewhere never assume directories
	// exist (doing so is a racing condition), it is valid to simply
	// handle errors at this level.
	if err := os.MkdirAll(path, 0700); err != nil {
		fmt.Fprintf(os.Stderr, "Error creating '%v': %v", path, err)
		debug.PrintStack()
	}
}
