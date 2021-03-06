/*
Copyright 2014 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"

	"github.com/golang/glog"
	"github.com/renstrom/dedent"
	"github.com/spf13/cobra"
	"k8s.io/kubernetes/pkg/api"
	"k8s.io/kubernetes/pkg/api/meta"
	"k8s.io/kubernetes/pkg/kubectl"
	cmdutil "k8s.io/kubernetes/pkg/kubectl/cmd/util"
	"k8s.io/kubernetes/pkg/kubectl/resource"
	"k8s.io/kubernetes/pkg/runtime"
	"k8s.io/kubernetes/pkg/util/strategicpatch"
)

// AnnotateOptions have the data required to perform the annotate operation
type AnnotateOptions struct {
	resource.FilenameOptions

	resources         []string
	newAnnotations    map[string]string
	removeAnnotations []string
	builder           *resource.Builder
	selector          string

	overwrite       bool
	all             bool
	resourceVersion string

	changeCause       string
	recordChangeCause bool

	f   *cmdutil.Factory
	out io.Writer
	cmd *cobra.Command
}

var (
	annotate_long = dedent.Dedent(`
		Update the annotations on one or more resources.

		An annotation is a key/value pair that can hold larger (compared to a label), and possibly not human-readable, data.
		It is intended to store non-identifying auxiliary data, especially data manipulated by tools and system extensions.
		If --overwrite is true, then existing annotations can be overwritten, otherwise attempting to overwrite an annotation will result in an error.
		If --resource-version is specified, then updates will use this resource version, otherwise the existing resource-version will be used.

		`) + valid_resources

	annotate_example = dedent.Dedent(`
		# Update pod 'foo' with the annotation 'description' and the value 'my frontend'.
		# If the same annotation is set multiple times, only the last value will be applied
		kubectl annotate pods foo description='my frontend'

		# Update a pod identified by type and name in "pod.json"
		kubectl annotate -f pod.json description='my frontend'

		# Update pod 'foo' with the annotation 'description' and the value 'my frontend running nginx', overwriting any existing value.
		kubectl annotate --overwrite pods foo description='my frontend running nginx'

		# Update all pods in the namespace
		kubectl annotate pods --all description='my frontend running nginx'

		# Update pod 'foo' only if the resource is unchanged from version 1.
		kubectl annotate pods foo description='my frontend running nginx' --resource-version=1

		# Update pod 'foo' by removing an annotation named 'description' if it exists.
		# Does not require the --overwrite flag.
		kubectl annotate pods foo description-`)
)

func NewCmdAnnotate(f *cmdutil.Factory, out io.Writer) *cobra.Command {
	options := &AnnotateOptions{}

	validArgs, argAliases := []string{}, []string{}
	p, err := f.Printer(nil, kubectl.PrintOptions{
		ColumnLabels: []string{},
	})
	cmdutil.CheckErr(err)
	if p != nil {
		validArgs = p.HandledResources()
		argAliases = kubectl.ResourceAliases(validArgs)
	}

	cmd := &cobra.Command{
		Use:     "annotate [--overwrite] (-f FILENAME | TYPE NAME) KEY_1=VAL_1 ... KEY_N=VAL_N [--resource-version=version]",
		Short:   "Update the annotations on a resource",
		Long:    annotate_long,
		Example: annotate_example,
		Run: func(cmd *cobra.Command, args []string) {
			if err := options.Complete(f, out, cmd, args); err != nil {
				cmdutil.CheckErr(err)
			}
			if err := options.Validate(args); err != nil {
				cmdutil.CheckErr(cmdutil.UsageError(cmd, err.Error()))
			}
			if err := options.RunAnnotate(); err != nil {
				cmdutil.CheckErr(err)
			}
		},
		ValidArgs:  validArgs,
		ArgAliases: argAliases,
	}
	cmdutil.AddPrinterFlags(cmd)
	cmdutil.AddInclude3rdPartyFlags(cmd)
	cmd.Flags().StringVarP(&options.selector, "selector", "l", "", "Selector (label query) to filter on")
	cmd.Flags().BoolVar(&options.overwrite, "overwrite", false, "If true, allow annotations to be overwritten, otherwise reject annotation updates that overwrite existing annotations.")
	cmd.Flags().BoolVar(&options.all, "all", false, "select all resources in the namespace of the specified resource types")
	cmd.Flags().StringVar(&options.resourceVersion, "resource-version", "", "If non-empty, the annotation update will only succeed if this is the current resource-version for the object. Only valid when specifying a single resource.")
	usage := "identifying the resource to update the annotation"
	cmdutil.AddFilenameOptionFlags(cmd, &options.FilenameOptions, usage)
	cmdutil.AddRecordFlag(cmd)
	return cmd
}

// Complete adapts from the command line args and factory to the data required.
func (o *AnnotateOptions) Complete(f *cmdutil.Factory, out io.Writer, cmd *cobra.Command, args []string) (err error) {

	namespace, enforceNamespace, err := f.DefaultNamespace()
	if err != nil {
		return err
	}

	// retrieves resource and annotation args from args
	// also checks args to verify that all resources are specified before annotations
	resources, annotationArgs, err := cmdutil.GetResourcesAndPairs(args, "annotation")
	if err != nil {
		return err
	}
	o.resources = resources
	if len(o.resources) < 1 && cmdutil.IsFilenameEmpty(o.Filenames) {
		return fmt.Errorf("one or more resources must be specified as <resource> <name> or <resource>/<name>")
	}
	if len(annotationArgs) < 1 {
		return fmt.Errorf("at least one annotation update is required")
	}

	if o.newAnnotations, o.removeAnnotations, err = parseAnnotations(annotationArgs); err != nil {
		return err
	}

	o.recordChangeCause = cmdutil.GetRecordFlag(cmd)
	o.changeCause = f.Command()

	mapper, typer := f.Object()
	o.builder = resource.NewBuilder(mapper, typer, resource.ClientMapperFunc(f.ClientForMapping), f.Decoder(true)).
		ContinueOnError().
		NamespaceParam(namespace).DefaultNamespace().
		FilenameParam(enforceNamespace, &o.FilenameOptions).
		SelectorParam(o.selector).
		ResourceTypeOrNameArgs(o.all, o.resources...).
		Flatten().
		Latest()

	o.f = f
	o.out = out
	o.cmd = cmd

	return nil
}

// Validate checks to the AnnotateOptions to see if there is sufficient information run the command.
func (o AnnotateOptions) Validate(args []string) error {
	return validateAnnotations(o.removeAnnotations, o.newAnnotations)
}

// RunAnnotate does the work
func (o AnnotateOptions) RunAnnotate() error {
	r := o.builder.Do()
	if err := r.Err(); err != nil {
		return err
	}

	var singularResource bool
	r.IntoSingular(&singularResource)

	// only apply resource version locking on a single resource.
	// we must perform this check after o.builder.Do() as
	// []o.resources can not not accurately return the proper number
	// of resources when they are not passed in "resource/name" format.
	if !singularResource && len(o.resourceVersion) > 0 {
		return fmt.Errorf("--resource-version may only be used with a single resource")
	}

	return r.Visit(func(info *resource.Info, err error) error {
		if err != nil {
			return err
		}

		obj, err := cmdutil.MaybeConvertObject(info.Object, info.Mapping.GroupVersionKind.GroupVersion(), info.Mapping)
		if err != nil {
			return err
		}
		name, namespace := info.Name, info.Namespace
		oldData, err := json.Marshal(obj)
		if err != nil {
			return err
		}
		// If we should record change-cause, add it to new annotations
		if cmdutil.ContainsChangeCause(info) || o.recordChangeCause {
			o.newAnnotations[kubectl.ChangeCauseAnnotation] = o.changeCause
		}
		if err := o.updateAnnotations(obj); err != nil {
			return err
		}
		newData, err := json.Marshal(obj)
		if err != nil {
			return err
		}
		patchBytes, err := strategicpatch.CreateTwoWayMergePatch(oldData, newData, obj)
		createdPatch := err == nil
		if err != nil {
			glog.V(2).Infof("couldn't compute patch: %v", err)
		}

		mapping := info.ResourceMapping()
		client, err := o.f.ClientForMapping(mapping)
		if err != nil {
			return err
		}
		helper := resource.NewHelper(client, mapping)

		var outputObj runtime.Object
		if createdPatch {
			outputObj, err = helper.Patch(namespace, name, api.StrategicMergePatchType, patchBytes)
		} else {
			outputObj, err = helper.Replace(namespace, name, false, obj)
		}
		if err != nil {
			return err
		}

		mapper, _ := o.f.Object()
		outputFormat := cmdutil.GetFlagString(o.cmd, "output")
		if outputFormat != "" {
			return o.f.PrintObject(o.cmd, mapper, outputObj, o.out)
		}

		cmdutil.PrintSuccess(mapper, false, o.out, info.Mapping.Resource, info.Name, false, "annotated")
		return nil
	})
}

// parseAnnotations retrieves new and remove annotations from annotation args
func parseAnnotations(annotationArgs []string) (map[string]string, []string, error) {
	return cmdutil.ParsePairs(annotationArgs, "annotation", true)
}

// validateAnnotations checks the format of annotation args and checks removed annotations aren't in the new annotations map
func validateAnnotations(removeAnnotations []string, newAnnotations map[string]string) error {
	var modifyRemoveBuf bytes.Buffer
	for _, removeAnnotation := range removeAnnotations {
		if _, found := newAnnotations[removeAnnotation]; found {
			if modifyRemoveBuf.Len() > 0 {
				modifyRemoveBuf.WriteString(", ")
			}
			modifyRemoveBuf.WriteString(fmt.Sprintf(removeAnnotation))
		}
	}
	if modifyRemoveBuf.Len() > 0 {
		return fmt.Errorf("can not both modify and remove the following annotation(s) in the same command: %s", modifyRemoveBuf.String())
	}

	return nil
}

// validateNoAnnotationOverwrites validates that when overwrite is false, to-be-updated annotations don't exist in the object annotation map (yet)
func validateNoAnnotationOverwrites(accessor meta.Object, annotations map[string]string) error {
	var buf bytes.Buffer
	for key := range annotations {
		// change-cause annotation can always be overwritten
		if key == kubectl.ChangeCauseAnnotation {
			continue
		}
		if value, found := accessor.GetAnnotations()[key]; found {
			if buf.Len() > 0 {
				buf.WriteString("; ")
			}
			buf.WriteString(fmt.Sprintf("'%s' already has a value (%s)", key, value))
		}
	}
	if buf.Len() > 0 {
		return fmt.Errorf("--overwrite is false but found the following declared annotation(s): %s", buf.String())
	}
	return nil
}

// updateAnnotations updates annotations of obj
func (o AnnotateOptions) updateAnnotations(obj runtime.Object) error {
	accessor, err := meta.Accessor(obj)
	if err != nil {
		return err
	}
	if !o.overwrite {
		if err := validateNoAnnotationOverwrites(accessor, o.newAnnotations); err != nil {
			return err
		}
	}

	annotations := accessor.GetAnnotations()
	if annotations == nil {
		annotations = make(map[string]string)
	}

	for key, value := range o.newAnnotations {
		annotations[key] = value
	}
	for _, annotation := range o.removeAnnotations {
		delete(annotations, annotation)
	}
	accessor.SetAnnotations(annotations)

	if len(o.resourceVersion) != 0 {
		accessor.SetResourceVersion(o.resourceVersion)
	}
	return nil
}
