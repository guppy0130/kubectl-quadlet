package main

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/cli-runtime/pkg/resource"
	v1 "k8s.io/client-go/applyconfigurations/core/v1"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"sigs.k8s.io/kustomize/kyaml/kio"
	"sigs.k8s.io/kustomize/kyaml/yaml"

	"github.com/containers/podman/v5/pkg/systemd/quadlet"
	"github.com/coreos/go-systemd/v22/unit"
)

var errFailedCast = errors.New("cast failure")

type QuadletOptions struct {
	PrintFlags      *genericclioptions.PrintFlags
	ConfigFlags     *genericclioptions.ConfigFlags
	FilenameOptions resource.FilenameOptions
	Logger          slog.Logger
	OutputDir       string
}

func NewQuadletOptions() *QuadletOptions {
	return &QuadletOptions{
		PrintFlags:      genericclioptions.NewPrintFlags("quadlet"),
		ConfigFlags:     genericclioptions.NewConfigFlags(true),
		FilenameOptions: resource.FilenameOptions{},
		Logger:          *slog.New(slog.NewTextHandler(os.Stderr, nil)),
		OutputDir:       "%E/containers/systemd",
	}
}

// TODO: handle many deployments?
func (q *QuadletOptions) Run(f cmdutil.Factory) error {
	// gets the resources from disk or other sources
	r := f.NewBuilder().Local().Unstructured().FilenameParam(false, &q.FilenameOptions).Flatten().Do()
	if err := r.Err(); err != nil {
		return err
	}
	// parse items and turn into things
	allErrs := []error{}
	infos, err := r.Infos()
	if err != nil {
		allErrs = append(allErrs, err)
	}
	if err := utilerrors.NewAggregate(allErrs); err != nil {
		return err
	}

	rNodes := make([]*yaml.RNode, 0, len(infos))

	// read each of the entries to figure out how to configure the systemd unit
	// TODO: make a bunch of visitors instead of this jank shit
	// TODO: figure out how to sort this so you can do port finding in a single pass; e.g., don't want to have deployment+pods come _after_ service
	unitOptions := make([]*unit.UnitOption, 0, 10)
	containerToHostPortMappings := make(map[int32]int32)
	deploymentName := ""
	var deployment appsv1.Deployment

	// TODO: figure out if this is stupid
	for ix := range infos {
		// default to adding it to the manifest?
		shouldBeAddedToManifest := true

		// ref to the object so we can treat it as unstructured
		obj := infos[ix].Object
		unstructuredObj, ok := obj.(runtime.Unstructured)
		// if we couldn't cast it, explode
		if !ok {
			return fmt.Errorf("%w: %s", errFailedCast, infos[ix].ObjectName())
		}

		// TODO: figure out if there's a constant for this
		if obj.GetObjectKind().GroupVersionKind().Kind == "Deployment" {
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(unstructuredObj.UnstructuredContent(), &deployment); err != nil {
				return err
			}
			deploymentName = deployment.Name
		}

		// get the exposed ports from the service and pass to the unit
		if obj.GetObjectKind().GroupVersionKind().Kind == "Service" {
			shouldBeAddedToManifest = false
			var service v1.ServiceApplyConfiguration
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(unstructuredObj.UnstructuredContent(), &service); err != nil {
				return err
			}
			// TODO: replace with something similar to
			// https://github.com/kubernetes/kubernetes/blob/46aa8959a0659e22c924bb52b38385d441715b2b/pkg/api/v1/pod/util.go#L32
			// we can't use it directly because we only have a PodSpec, not a Pod
			for _, port := range service.Spec.Ports {
				if port.TargetPort != nil { //nolint:gocritic // not switching on same variable
					containerToHostPortMappings[*port.Port] = port.TargetPort.IntVal
				} else if port.NodePort != nil {
					containerToHostPortMappings[*port.Port] = *port.NodePort
				} else {
					containerToHostPortMappings[*port.Port] = *port.Port
				}
			}
		}

		// TODO: ask quadlet for what types it can handle
		// don't add types that quadlet can't handle
		if shouldBeAddedToManifest {
			rnode, err := yaml.FromMap(unstructuredObj.UnstructuredContent())
			if err != nil {
				return err
			}
			rNodes = append(rNodes, rnode)
		}
	}
	yamlOutputFile := fmt.Sprintf("%s.full_manifest.yaml", deploymentName)
	unitOutputFIle := fmt.Sprintf("%s.kube", deploymentName)

	yamlOutput, err := os.Create(yamlOutputFile)
	if err != nil {
		return err
	}
	writer := kio.ByteWriter{
		Sort:   true,
		Writer: yamlOutput,
	}
	if err := writer.Write(rNodes); err != nil {
		return err
	}

	unitOptions = append(
		unitOptions,
		unit.NewUnitOption(quadlet.UnitGroup, "Name", deploymentName),
		unit.NewUnitOption(quadlet.UnitGroup, "Description", deploymentName),
		unit.NewUnitOption(quadlet.KubeGroup, quadlet.KeyYaml, filepath.Join(q.OutputDir, deploymentName, yamlOutputFile)),
		// TODO: figure out if start on boot is desirable
		unit.NewUnitOption(quadlet.InstallGroup, "WantedBy", "multi-user.target"),
	)
	for containerPort, hostPort := range containerToHostPortMappings {
		// TODO: figure out if should listen on all interfaces
		s := fmt.Sprintf("%d:%d", hostPort, containerPort)
		unitOptions = append(unitOptions, unit.NewUnitOption(quadlet.KubeGroup, quadlet.KeyPublishPort, s))
	}
	unitOutput, err := os.Create(unitOutputFIle)
	if err != nil {
		return err
	}
	if _, err = io.Copy(unitOutput, unit.Serialize(unitOptions)); err != nil {
		return err
	}

	return nil
}

func (q *QuadletOptions) Validate() error {
	if err := q.FilenameOptions.RequireFilenameOrKustomize(); err != nil {
		return err
	}
	// additional validations go here, if any
	return nil
}

func NewCmdQuadlet() *cobra.Command {
	o := NewQuadletOptions()

	cmd := &cobra.Command{
		Use:   "kubectl quadlet",
		Short: "create quadlet files from kubernetes manifests",
		Long: "Outputs all kinds into a single file (`%n.full_manifest.yaml`) and a `%n.kube` file for quadlet. " +
			"You should place the quadlet file in `%E/containers/systemd/` " +
			"and the aggregated kinds file into `%E/containers/systemd/%n/`",
		SilenceErrors: true,
		SilenceUsage:  true,
		PreRunE: func(cmd *cobra.Command, args []string) error {
			if err := viper.BindPFlags(cmd.Flags()); err != nil {
				return err
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			cmdutil.CheckErr(o.Validate())

			f := cmdutil.NewFactory(o.ConfigFlags)
			cmdutil.CheckErr(o.Run(f))
			return nil
		},
	}

	cobra.OnInitialize(initConfig)

	cmdutil.AddFilenameOptionFlags(cmd, &o.FilenameOptions, "to consider for quadlet-izing")
	o.ConfigFlags.AddFlags(cmd.Flags())
	o.PrintFlags.AddFlags(cmd)

	viper.SetEnvKeyReplacer(strings.NewReplacer("-", "_"))
	return cmd
}

func initConfig() {
	viper.AutomaticEnv()
}

func main() {
	if err := NewCmdQuadlet().Execute(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}
