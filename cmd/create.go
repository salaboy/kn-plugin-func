package cmd

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"text/template"

	"github.com/AlecAivazis/survey/v2"
	"github.com/ory/viper"
	"github.com/spf13/cobra"

	fn "knative.dev/kn-plugin-func"
	"knative.dev/kn-plugin-func/utils"
)

// ErrNoRuntime indicates that the language runtime flag was not passed.
type ErrNoRuntime error

// ErrInvalidRuntime indicates that the passed language runtime was invalid.
type ErrInvalidRuntime error

// ErrInvalidTemplate indicates that the passed template was invalid.
type ErrInvalidTemplate error

// NewCreateCmd creates a create command using the given client creator.
func NewCreateCmd(newClient ClientFactory) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a Function Project",
		Long: `
NAME
	{{.Name}} create - Create a Function project.

SYNOPSIS
	{{.Name}} create [-l|--language] [-t|--template] [-r|--repository]
	            [-c|--confirm]  [-v|--verbose]  [path]

DESCRIPTION
	Creates a new Function project.

	  $ {{.Name}} create -l node -t http

	Creates a Function in the current directory '.' which is written in the
	language/runtime 'node' and handles HTTP events.

	If [path] is provided, the Function is initialized at that path, creating
	the path if necessary.

	To complete this command interactivly, use --confirm (-c):
	  $ {{.Name}} create -c

	Available Language Runtimes and Templates:
{{ .Options | indent 2 " " | indent 1 "\t" }}

	To install more language runtimes and their templates see '{{.Name}} repository'.


EXAMPLES
	o Create a Node.js Function (the default language runtime) in the current
	  directory (the default path) which handles http events (the default
	  template).
	  $ {{.Name}} create

	o Create a Node.js Function in the directory 'myfunc'.
	  $ {{.Name}} create myfunc

	o Create a Go Function which handles CloudEvents in ./myfunc.
	  $ {{.Name}} create -l go -t cloudevents myfunc
		`,
		SuggestFor: []string{"vreate", "creaet", "craete", "new"},
		PreRunE:    bindEnv("language", "template", "repository", "confirm"),
	}

	// Flags
	cmd.Flags().StringP("language", "l", "", "Language Runtime (see help text for list) (Env: $FUNC_LANGUAGE)")
	cmd.Flags().StringP("template", "t", fn.DefaultTemplate, "Function template. (see help text for list) (Env: $FUNC_TEMPLATE)")
	cmd.Flags().StringP("repository", "r", "", "URI to a Git repository containing the specified template (Env: $FUNC_REPOSITORY)")
	cmd.Flags().BoolP("confirm", "c", false, "Prompt to confirm all options interactively (Env: $FUNC_CONFIRM)")

	// Help Action
	cmd.SetHelpFunc(func(cmd *cobra.Command, args []string) { runCreateHelp(cmd, args, newClient) })

	// Run Action
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		return runCreate(cmd, args, newClient)
	}

	// Tab completion
	if err := cmd.RegisterFlagCompletionFunc("language", newRuntimeCompletionFunc(newClient)); err != nil {
		fmt.Fprintf(os.Stderr, "unable to provide language runtime suggestions: %v", err)
	}
	if err := cmd.RegisterFlagCompletionFunc("template", newTemplateCompletionFunc(newClient)); err != nil {
		fmt.Fprintf(os.Stderr, "unable to provide template suggestions: %v", err)
	}

	return cmd
}

// Run Create
func runCreate(cmd *cobra.Command, args []string, newClient ClientFactory) (err error) {
	// Config
	// Create a config based on args.  Also uses the newClient to create a
	// temporary client for completing options such as available runtimes.
	cfg, err := newCreateConfig(cmd, args, newClient)
	if err != nil {
		return
	}

	// Client
	// From environment variables, flags, arguments, and user prompts if --confirm
	// (in increasing levels of precidence)
	client, done := newClient(ClientConfig{Verbose: cfg.Verbose},
		fn.WithRepository(cfg.Repository),             // Use exactly this repo OR
		fn.WithRepositoriesPath(cfg.RepositoriesPath)) // Path on disk to installed repos
	defer done()

	// Validate - a deeper validation than that which is performed when
	// instantiating the client with the raw config above.
	if err = cfg.Validate(client); err != nil {
		return
	}

	// Create
	err = client.Create(fn.Function{
		Name:     cfg.Name,
		Root:     cfg.Path,
		Runtime:  fn.FunctionRuntimeSpec{Runtime: cfg.Runtime},
		Template: cfg.Template,
	})
	if err != nil {
		return err
	}

	// Confirm
	fmt.Fprintf(cmd.OutOrStderr(), "Created %v Function in %v\n", cfg.Runtime, cfg.Path)
	return nil
}

// Run Help
func runCreateHelp(cmd *cobra.Command, args []string, newClient ClientFactory) {
	// Error-tolerant implementation:
	// Help can not fail when creating the client config (such as on invalid
	// flag values) because help text is needed in that situation.   Therefore,
	// this implementation must be resilient to cfg zero value.
	failSoft := func(err error) {
		if err != nil {
			fmt.Fprintf(cmd.OutOrStderr(), "error: help text may be partial: %v", err)
		}
	}

	tpl := createHelpTemplate(cmd)

	cfg, err := newCreateConfig(cmd, args, newClient)
	failSoft(err)

	client, done := newClient(ClientConfig{Verbose: cfg.Verbose},
		fn.WithRepositoriesPath(cfg.RepositoriesPath),
		fn.WithRepository(cfg.Repository))
	defer done()

	options, err := runtimeTemplateOptions(client) // human-friendly
	failSoft(err)

	var data = struct {
		Options string
		Name    string
	}{
		Options: options,
		Name:    cmd.Root().Use,
	}
	if err := tpl.Execute(cmd.OutOrStdout(), data); err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "unable to display help text: %v", err)
	}
}

type createConfig struct {
	Path       string // Absolute path to Function sourcsource
	Runtime    string // Language Runtime
	Repository string // Repository URI (overrides builtin and installed)
	Verbose    bool   // Verbose output
	Confirm    bool   // Confirm values via an interactive prompt

	// Template is the code written into the new Function project, including
	// an implementation adhering to one of the supported function signatures.
	// May also include additional configuration settings or examples.
	// For example, embedded are 'http' for a Function whose function signature
	// is invoked via straight HTTP requests, or 'events' for a Function which
	// will be invoked with CloudEvents.  These embedded templates contain a
	// minimum implementation of the signature itself and example tests.
	Template string

	// Name of the Function
	Name string

	// RepositoriesPath is an optional configuration setting (not set via flags)
	// which overrides the location on disk at which to search for installed
	// repositories.
	// Override using $FUNC_REPOSITORIES_PATH
	// Default value is $XDG_CONFIG_HOME/func/repositories
	// (~/.config/func/repositories)
	RepositoriesPath string
}

// newCreateConfig returns a config populated from the current execution context
// (args, flags and environment variables)
// The client constructor function is used to create a transient client for
// accessing things like the current valid templates list, and uses the
// current value of the config at time of prompting.
func newCreateConfig(cmd *cobra.Command, args []string, newClient ClientFactory) (cfg createConfig, err error) {
	var (
		path             string
		dirName          string
		absolutePath     string
		repositoriesPath string
	)

	if len(args) >= 1 {
		path = args[0]
	}

	// Convert the path to an absolute path, and extract the ending directory name
	// as the function name. TODO: refactor to be git-like with no name up-front
	// and set instead as a named one-to-many deploy target.
	dirName, absolutePath = deriveNameAndAbsolutePathFromPath(path)

	// Repositories Path
	// Not exposed as a flag due to potential confusion with the more likely
	// "repository override" flag, and due to its unlikliness of being needed, but
	// it is still available as an environment variable.
	repositoriesPath = os.Getenv("FUNC_REPOSITORIES_PATH")
	if repositoriesPath == "" { // if no env var provided
		repositoriesPath = fn.New().RepositoriesPath() // use ~/.config/func/repositories
	}

	// Config is the final default values based off the execution context.
	// When prompting, these become the defaults presented.
	cfg = createConfig{
		Name:             dirName, // TODO: refactor to be git-like
		Path:             absolutePath,
		RepositoriesPath: repositoriesPath,
		Repository:       viper.GetString("repository"),
		Runtime:          viper.GetString("language"), // users refer to it is language
		Template:         viper.GetString("template"),
		Confirm:          viper.GetBool("confirm"),
		Verbose:          viper.GetBool("verbose"),
	}
	// If not in confirm/prompting mode, this cfg structure is complete.
	if !cfg.Confirm {
		return
	}

	// Create a tempoarary client for use by the following prompts to complete
	// runtime/template suggestions etc
	client, done := newClient(ClientConfig{Verbose: cfg.Verbose})
	defer done()

	// IN confirm mode.  If also in an interactive terminal, run prompts.
	if interactiveTerminal() {
		createdCfg, err := cfg.prompt(client)
		if err != nil {
			return createdCfg, err
		}
		fmt.Println("Command:")
		fmt.Println(singleCommand(cmd, args, createdCfg))
		return createdCfg, nil
	}

	// Confirming, but noninteractive
	// Print out the final values as a confirmation.  Only show Repository or
	// Repositories, not both (repository takes precidence) in order to avoid
	// likely confusion if both are displayed and one is empty.
	// be removed and both displayed.
	fmt.Printf("Path:         %v\n", cfg.Path)
	fmt.Printf("Language:     %v\n", cfg.Runtime) // users refer to it as language
	if cfg.Repository != "" {                     // if an override was provided
		fmt.Printf("Repository:   %v\n", cfg.Repository) // show only the override
	}
	fmt.Printf("Template:     %v\n", cfg.Template)
	return
}

// singleCommand that could be used by the current user to minimally recreate the current state.
func singleCommand(cmd *cobra.Command, args []string, cfg createConfig) string {
	var b strings.Builder
	b.WriteString(cmd.Root().Name())    // process executable
	b.WriteString(" -l " + cfg.Runtime) // language runtime is required
	if cmd.Flags().Lookup("template").Changed {
		b.WriteString(" -t " + cfg.Template)
	}
	if cmd.Flags().Lookup("repository").Changed {
		b.WriteString(" -r " + cfg.Repository)
	}
	if cmd.Flags().Lookup("verbose").Changed {
		b.WriteString(fmt.Sprintf(" -v %v", cfg.Verbose))
	}
	if len(args) > 0 {
		b.WriteString(" " + cfg.Path) // optional trailing <path> argument
	}
	return b.String()
}

// Validate the current state of the config, returning any errors.
// Note this is a deeper validation using a client already configured with a
// preliminary config object from flags/config, such that the client instance
// can be used to determine possible values for runtime, templates, etc.  a
// pre-client validation should not be required, as the Client does its own
// validation.
func (c createConfig) Validate(client *fn.Client) (err error) {

	// Confirm Name is valid
	// Note that this is highly constricted, as it must currently adhere to the
	// naming of a Knative Service, which itself is constrained to a Kubernetes
	// Service, which itself is constrained to a DNS label (a subdomain).
	// TODO: refactor to be git-like with no name at time of creation, but rather
	// with named deployment targets in a one-to-many configuration.
	dirName, _ := deriveNameAndAbsolutePathFromPath(c.Path)
	if err = utils.ValidateFunctionName(dirName); err != nil {
		return
	}

	// Validate Runtime and Template Name
	//
	// Perhaps additional validation would be of use here in the CLI, but
	// the client libray itself is ultimately responsible for validating all input
	// prior to exeuting any requests.
	// Client validates both language runtime and template exist, with language runtime
	// being a mandatory flag while defaulting template if not present to 'http'.
	// However, if either of them are invalid, or the chosen combination does not exist,
	// the error message is a rather terse one-liner. This is suitable for libraries, but
	// for a CLI it behooves us to be more verbose, including valid options for
	// each.  So here, we check that the values entered (if any) are both valid
	// and valid together.
	if c.Runtime == "" {
		return noRuntimeError(client)
	}
	if c.Runtime != "" && c.Repository == "" &&
		!isValidRuntime(client, c.Runtime) {
		return newInvalidRuntimeError(client, c.Runtime)
	}

	if c.Template != "" && c.Repository == "" &&
		!isValidTemplate(client, c.Runtime, c.Template) {
		return newInvalidTemplateError(client, c.Runtime, c.Template)
	}

	return
}

// isValidRuntime determines if the given language runtime is a valid choice.
func isValidRuntime(client *fn.Client, runtime string) bool {
	runtimes, err := client.Runtimes()
	if err != nil {
		return false
	}
	for _, v := range runtimes {
		if v == runtime {
			return true
		}
	}
	return false
}

// isValidTemplate determines if the given template is valid for the given
// runtime.
func isValidTemplate(client *fn.Client, runtime, template string) bool {
	if !isValidRuntime(client, runtime) {
		return false
	}
	templates, err := client.Templates().List(runtime)
	if err != nil {
		return false
	}
	for _, v := range templates {
		if v == template {
			return true
		}
	}
	return false
}

// noRuntimeError creates an error stating that the language flag
// is required, and a verbose list of valid options.
func noRuntimeError(client *fn.Client) error {
	b := strings.Builder{}
	fmt.Fprintf(&b, "Required flag \"language\" not set.\n")
	fmt.Fprintln(&b, "Available language runtimes are:")
	runtimes, err := client.Runtimes()
	if err != nil {
		return err
	}
	for _, v := range runtimes {
		fmt.Fprintf(&b, "  %v\n", v)
	}
	return ErrNoRuntime(errors.New(b.String()))
}

// newInvalidRuntimeError creates an error stating that the given language
// is not valid, and a verbose list of valid options.
func newInvalidRuntimeError(client *fn.Client, runtime string) error {
	b := strings.Builder{}
	fmt.Fprintf(&b, "The language runtime '%v' is not recognized.\n", runtime)
	fmt.Fprintln(&b, "Available language runtimes are:")
	runtimes, err := client.Runtimes()
	if err != nil {
		return err
	}
	for _, v := range runtimes {
		fmt.Fprintf(&b, "  %v\n", v)
	}
	return ErrInvalidRuntime(errors.New(b.String()))
}

// newInvalidTemplateError creates an error stating that the given template
// is not available for the given runtime, and a verbose list of valid options.
// The runtime is expected to already have been validated.
func newInvalidTemplateError(client *fn.Client, runtime, template string) error {
	b := strings.Builder{}
	fmt.Fprintf(&b, "The template '%v' was not found for language runtime '%v'.\n", template, runtime)
	fmt.Fprintln(&b, "Available templates for this language runtime are:")
	templates, err := client.Templates().List(runtime)
	if err != nil {
		return err
	}
	for _, v := range templates {
		fmt.Fprintf(&b, "  %v\n", v)
	}
	return ErrInvalidTemplate(errors.New(b.String()))
}

// prompt the user with value of config members, allowing for interactively
// mutating the values. The provided clientFn is used to construct a transient
// client for use during prompt autocompletion/suggestions (such as suggesting
// valid templates)
func (c createConfig) prompt(client *fn.Client) (createConfig, error) {
	var qs []*survey.Question

	runtimes, err := client.Runtimes()
	if err != nil {
		return createConfig{}, err
	}

	// First ask for path...
	qs = []*survey.Question{
		{
			Name: "Path",
			Prompt: &survey.Input{
				Message: "Function Path:",
				Default: c.Path,
			},
			Validate: func(val interface{}) error {
				derivedName, _ := deriveNameAndAbsolutePathFromPath(val.(string))
				return utils.ValidateFunctionName(derivedName)
			},
			Transform: func(ans interface{}) interface{} {
				_, absolutePath := deriveNameAndAbsolutePathFromPath(ans.(string))
				return absolutePath
			},
		}, {
			Name: "Runtime",
			Prompt: &survey.Select{
				Message: "Language Runtime:",
				Options: runtimes,
				Default: surveySelectDefault(c.Runtime, runtimes),
			},
		}}
	if err := survey.Ask(qs, &c); err != nil {
		return c, err
	}

	// Second loop: choose template with autocompletion filtered by chosen runtime
	qs = []*survey.Question{
		{
			Name: "Template",
			Prompt: &survey.Input{
				Message: "Template:",
				Default: c.Template,
				Suggest: func(prefix string) []string {
					suggestions, err := templatesWithPrefix(prefix, c.Runtime, client)
					if err != nil {
						fmt.Fprintf(os.Stderr, "unable to suggest: %v", err)
					}
					return suggestions
				},
			},
		},
	}
	if err := survey.Ask(qs, &c); err != nil {
		return c, err
	}

	return c, nil
}

// Tab Completion and Prompt Suggestions Helpers
// ---------------------------------------------

type flagCompletionFunc func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective)

func newRuntimeCompletionFunc(newClient ClientFactory) flagCompletionFunc {
	return func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		cfg, err := newCreateConfig(cmd, args, newClient)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error creating client config for flag completion: %v", err)
		}
		client, done := newClient(ClientConfig{Verbose: cfg.Verbose})
		defer done()
		return CompleteRuntimeList(cmd, args, toComplete, client)
	}
}

func newTemplateCompletionFunc(newClient ClientFactory) flagCompletionFunc {
	return func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		cfg, err := newCreateConfig(cmd, args, newClient)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error creating client config for flag completion: %v", err)
		}
		client, done := newClient(ClientConfig{Verbose: cfg.Verbose})
		defer done()
		return CompleteTemplateList(cmd, args, toComplete, client)
	}
}

// return templates for language runtime whose full name (including repository)
// have the given prefix.
func templatesWithPrefix(prefix, runtime string, client *fn.Client) ([]string, error) {
	var (
		suggestions    = []string{}
		templates, err = client.Templates().List(runtime)
	)
	if err != nil {
		return suggestions, err
	}
	for _, template := range templates {
		if strings.HasPrefix(template, prefix) {
			suggestions = append(suggestions, template)
		}
	}
	return suggestions, nil
}

// Template Helpers
// ---------------

// createHelpTemplate is the template for the create command help
func createHelpTemplate(cmd *cobra.Command) *template.Template {
	body := cmd.Long + "\n\n" + cmd.UsageString()
	t := template.New("help")
	fm := template.FuncMap{
		"indent": func(i int, c string, v string) string {
			indentation := strings.Repeat(c, i)
			return indentation + strings.Replace(v, "\n", "\n"+indentation, -1)
		},
	}
	t.Funcs(fm)
	return template.Must(t.Parse(body))
}

// runtimeTemplateOptions is a human-friendly table of valid Language Runtime
// to Template combinations.
func runtimeTemplateOptions(client *fn.Client) (string, error) {
	runtimes, err := client.Runtimes()
	if err != nil {
		return "", err
	}
	builder := strings.Builder{}
	writer := tabwriter.NewWriter(&builder, 0, 0, 3, ' ', 0)

	fmt.Fprint(writer, "Language\tTemplate\n")
	fmt.Fprint(writer, "--------\t--------\n")
	for _, r := range runtimes {
		templates, err := client.Templates().List(r)
		// Not all language packs will have templates for
		// all available runtimes. Without this check
		if err != nil && !errors.Is(err, fn.ErrTemplateNotFound) {
			return "", err
		}
		for _, t := range templates {
			fmt.Fprintf(writer, "%v\t%v\n", r, t) // write tabbed
		}
	}
	writer.Flush()
	return builder.String(), nil
}
