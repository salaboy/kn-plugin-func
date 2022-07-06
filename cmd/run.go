package cmd

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/ory/viper"
	"github.com/spf13/cobra"
	"knative.dev/client/pkg/util"

	fn "knative.dev/kn-plugin-func"
)

func NewRunCmd(newClient ClientFactory) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Run the function locally",
		Long: `Run the function locally

Runs the function locally in the current directory or in the directory
specified by --path flag.

Building
By default the Function will be built if never built, or if changes are detected
to the Function's source.  Use --build to override this behavior.

`,
		Example: `
# Run the Function locally, building if necessary
{{.Name}} run

# Run the Function, forcing a rebuild of the image.
#   This is useful when the Function's image was manually deleted, necessitating
#   A rebuild even when no changes have been made the Function's source.
{{.Name}} run --build

# Run the Function's existing image, disabling auto-build.
#   This is useful when filesystem changes have been made, but one wishes to
#   run the previously built image without rebuilding.
{{.Name}} run --build=false

`,
		SuggestFor: []string{"rnu"},
		PreRunE:    bindEnv("build", "path", "registry"),
	}

	cmd.Flags().StringArrayP("env", "e", []string{},
		"Environment variable to set in the form NAME=VALUE. "+
			"You may provide this flag multiple times for setting multiple environment variables. "+
			"To unset, specify the environment variable name followed by a \"-\" (e.g., NAME-).")
	cmd.Flags().StringP("build", "b", "auto", "Build the function. [auto|true|false].")
	cmd.Flags().Lookup("build").NoOptDefVal = "true" // --build is equivalient to --build=true
	cmd.Flags().StringP("registry", "r", GetDefaultRegistry(), "Registry + namespace part of the image if building, ex 'quay.io/myuser' (Env: $FUNC_REGISTRY)")
	setPathFlag(cmd)

	cmd.SetHelpFunc(defaultTemplatedHelp)

	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		return runRun(cmd, args, newClient)
	}

	return cmd
}

func runRun(cmd *cobra.Command, args []string, newClient ClientFactory) (err error) {
	config, err := newRunConfig(cmd)
	if err != nil {
		return
	}

	function, err := fn.NewFunction(config.Path)
	if err != nil {
		return
	}

	var updated int
	function.Run.Envs, updated, err = mergeEnvs(function.Run.Envs, config.EnvToUpdate, config.EnvToRemove)
	if err != nil {
		return
	}
	if updated > 0 {
		err = function.Write()
		if err != nil {
			return
		}
	}

	// Check if the Function has been initialized
	if !function.Initialized() {
		return fmt.Errorf("the given path '%v' does not contain an initialized function", config.Path)
	}

	// Client for use running (and potentially building), using the config
	// gathered plus any additional option overrieds (such as for providing
	// mocks when testing for builder and runner)
	client, done := newClient(ClientConfig{Verbose: config.Verbose}, fn.WithRegistry(config.Registry))
	defer done()

	// Build?
	// If --build was set to 'auto', only build if client detects the Function
	// is stale (has either never been built or has had filesystem modifications
	// since the last build).
	if config.Build == "auto" {
		if !client.Built(function.Root) {
			if err = client.Build(cmd.Context(), config.Path); err != nil {
				return
			}
		}
		fmt.Println("Detected Function was already built.  Use --build to override this behavior.")
		// Otherwise, --build should parse to a truthy value which indicates an explicit
		// override.
	} else {
		build, err := strconv.ParseBool(config.Build)
		if err != nil {
			return fmt.Errorf("invalid value for --build '%v'.  accepts 'auto', 'true' or 'false' (or similarly truthy value)", build)
		}
		if build {
			if err = client.Build(cmd.Context(), config.Path); err != nil {
				return err
			}
		} else {
			fmt.Println("Function build disabled.  Skipping build.")
		}

	}

	// Run the Function at path
	job, err := client.Run(cmd.Context(), config.Path)
	if err != nil {
		return
	}
	defer job.Stop()

	fmt.Fprintf(cmd.OutOrStderr(), "Function started on port %v\n", job.Port)

	select {
	case <-cmd.Context().Done():
		if !errors.Is(cmd.Context().Err(), context.Canceled) {
			err = cmd.Context().Err()
		}
		return
	case err = <-job.Errors:
		return
	}
}

type runConfig struct {
	// Path of the Function implementation on local disk. Defaults to current
	// working directory of the process.
	Path string

	// Verbose logging.
	Verbose bool

	// Envs passed via cmd to be added/updated
	EnvToUpdate *util.OrderedMap

	// Envs passed via cmd to removed
	EnvToRemove []string

	// Perform build.  Acceptable values are the keyword 'auto', or a truthy
	// value such as 'true', 'false, '1' or '0'.
	Build string

	// Registry for the build tag if building
	Registry string
}

func newRunConfig(cmd *cobra.Command) (c runConfig, err error) {
	envToUpdate, envToRemove, err := envFromCmd(cmd)
	if err != nil {
		return
	}

	return runConfig{
		Build:       viper.GetString("build"),
		Path:        viper.GetString("path"),
		Verbose:     viper.GetBool("verbose"), // defined on root
		Registry:    viper.GetString("registry"),
		EnvToUpdate: envToUpdate,
		EnvToRemove: envToRemove,
	}, nil
}
