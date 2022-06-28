package cmd

import (
	"context"
	"fmt"
	"strings"

	"github.com/AlecAivazis/survey/v2"
	"github.com/AlecAivazis/survey/v2/terminal"
	"github.com/spf13/cobra"

	fn "knative.dev/kn-plugin-func"
	"knative.dev/kn-plugin-func/k8s"
)

func NewConfigVolumesCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "volumes",
		Short: "List and manage configured volumes for a function",
		Long: `List and manage configured volumes for a function

Prints configured Volume mounts for a function project present in
the current directory or from the directory specified with --path.
`,
		SuggestFor: []string{"volums", "volume", "vols"},
		PreRunE:    bindEnv("path"),
		RunE: func(cmd *cobra.Command, args []string) (err error) {
			function, err := initConfigCommand(args, defaultLoaderSaver)
			if err != nil {
				return
			}

			listVolumes(function)

			return
		},
	}
	cmd.SetHelpFunc(defaultTemplatedHelp)

	configVolumesAddCmd := NewConfigVolumesAddCmd()
	configVolumesRemoveCmd := NewConfigVolumesRemoveCmd()

	setPathFlag(cmd)
	setPathFlag(configVolumesAddCmd)
	setPathFlag(configVolumesRemoveCmd)

	cmd.AddCommand(configVolumesAddCmd)
	cmd.AddCommand(configVolumesRemoveCmd)

	return cmd
}

func NewConfigVolumesAddCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "add",
		Short: "Add volume to the function configuration",
		Long: `Add volume to the function configuration

Interactive prompt to add Secrets and ConfigMaps as Volume mounts to the function project
in the current directory or from the directory specified with --path.
`,
		SuggestFor: []string{"ad", "create", "insert", "append"},
		PreRunE:    bindEnv("path"),
		RunE: func(cmd *cobra.Command, args []string) (err error) {
			function, err := initConfigCommand(args, defaultLoaderSaver)
			if err != nil {
				return
			}

			return runAddVolumesPrompt(cmd.Context(), function)
		},
	}
	cmd.SetHelpFunc(defaultTemplatedHelp)
	return cmd
}

func NewConfigVolumesRemoveCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "remove",
		Short: "Remove volume from the function configuration",
		Long: `Remove volume from the function configuration

Interactive prompt to remove Volume mounts from the function project
in the current directory or from the directory specified with --path.
`,
		SuggestFor: []string{"del", "delete", "rmeove"},
		PreRunE:    bindEnv("path"),
		RunE: func(cmd *cobra.Command, args []string) (err error) {
			function, err := initConfigCommand(args, defaultLoaderSaver)
			if err != nil {
				return
			}

			return runRemoveVolumesPrompt(function)
		},
	}
	cmd.SetHelpFunc(defaultTemplatedHelp)
	return cmd
}

func listVolumes(f fn.Function) {
	if len(f.Runtime.Volumes) == 0 {
		fmt.Println("There aren't any configured Volume mounts")
		return
	}

	fmt.Println("Configured Volumes mounts:")
	for _, v := range f.Runtime.Volumes {
		fmt.Println(" - ", v.String())
	}
}

func runAddVolumesPrompt(ctx context.Context, f fn.Function) (err error) {

	secrets, err := k8s.ListSecretsNamesIfConnected(ctx, f.Runtime.Namespace)
	if err != nil {
		return
	}
	configMaps, err := k8s.ListConfigMapsNamesIfConnected(ctx, f.Runtime.Namespace)
	if err != nil {
		return
	}

	// SECTION - select resource type to be mounted
	options := []string{}
	selectedOption := ""
	const optionConfigMap = "ConfigMap"
	const optionSecret = "Secret"

	if len(configMaps) > 0 {
		options = append(options, optionConfigMap)
	}
	if len(secrets) > 0 {
		options = append(options, optionSecret)
	}

	switch len(options) {
	case 0:
		fmt.Printf("There aren't any Secrets or ConfiMaps in the namespace \"%s\"\n", f.Runtime.Namespace)
		return
	case 1:
		selectedOption = options[0]
	case 2:
		err = survey.AskOne(&survey.Select{
			Message: "What do you want to mount as a Volume?",
			Options: options,
		}, &selectedOption)
		if err != nil {
			if err == terminal.InterruptErr {
				return nil
			}
			return
		}
	}

	// SECTION - select the specific resource to be mounted
	optionsResoures := []string{}
	resourceType := ""
	switch selectedOption {
	case optionConfigMap:
		resourceType = optionConfigMap
		optionsResoures = configMaps
	case optionSecret:
		resourceType = optionSecret
		optionsResoures = secrets
	}

	selectedResource := ""
	err = survey.AskOne(&survey.Select{
		Message: fmt.Sprintf("Which \"%s\" do you want to mount?", resourceType),
		Options: optionsResoures,
	}, &selectedResource)
	if err != nil {
		if err == terminal.InterruptErr {
			return nil
		}
		return
	}

	// SECTION - specify mount Path of the Volume

	path := ""
	err = survey.AskOne(&survey.Input{
		Message: fmt.Sprintf("Please specify the path where the %s should be mounted:", resourceType),
	}, &path, survey.WithValidator(func(val interface{}) error {
		if str, ok := val.(string); !ok || len(str) <= 0 || !strings.HasPrefix(str, "/") {
			return fmt.Errorf("The input must be non-empty absolute path.")
		}
		return nil
	}))
	if err != nil {
		if err == terminal.InterruptErr {
			return nil
		}
		return
	}

	// we have all necessary information -> let's store the new Volume
	newVolume := fn.Volume{Path: &path}
	switch selectedOption {
	case optionConfigMap:
		newVolume.ConfigMap = &selectedResource
	case optionSecret:
		newVolume.Secret = &selectedResource
	}

	f.Runtime.Volumes = append(f.Runtime.Volumes, newVolume)

	err = f.Write()
	if err == nil {
		fmt.Println("Volume entry was added to the function configuration")
	}

	return
}

func runRemoveVolumesPrompt(f fn.Function) (err error) {
	if len(f.Runtime.Volumes) == 0 {
		fmt.Println("There aren't any configured Volume mounts")
		return
	}

	options := []string{}
	for _, v := range f.Runtime.Volumes {
		options = append(options, v.String())
	}

	selectedVolume := ""
	prompt := &survey.Select{
		Message: "Which Volume do you want to remove?",
		Options: options,
	}
	err = survey.AskOne(prompt, &selectedVolume)
	if err != nil {
		if err == terminal.InterruptErr {
			return nil
		}
		return
	}

	var newVolumes []fn.Volume
	removed := false
	for i, v := range f.Runtime.Volumes {
		if v.String() == selectedVolume {
			newVolumes = append(f.Runtime.Volumes[:i], f.Runtime.Volumes[i+1:]...)
			removed = true
			break
		}
	}

	if removed {
		f.Runtime.Volumes = newVolumes
		err = f.Write()
		if err == nil {
			fmt.Println("Volume entry was removed from the function configuration")
		}
	}

	return
}
