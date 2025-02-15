package wrap

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/tektoncd/pipeline/pkg/apis/pipeline/v1beta1"
	clientset "github.com/tektoncd/pipeline/pkg/client/clientset/versioned"
	pipelineclient "github.com/tektoncd/pipeline/pkg/client/injection/client"
	"github.com/tektoncd/pipeline/pkg/resolution/common"
	"github.com/tektoncd/pipeline/pkg/resolution/resolver/framework"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/kubernetes"
	"knative.dev/pkg/client/injection/kube/client"
	"knative.dev/pkg/logging"
	"sigs.k8s.io/yaml"
)

// LabelValueWrapResolverType is the value to use for the
// resolution.tekton.dev/type label on resource requests
const LabelValueWrapResolverType string = "wrap"

// TODO(sbwsg): This should be exposed as a configurable option for
// admins (e.g. via ConfigMap)
const timeoutDuration = time.Minute

const (
	PipelineRefParam = "pipelineref"
	WorkspacesParam  = "workspaces"
	TargetParam      = "target"
	WrapperParam     = "wrapper"

	DefaultBaseImage = "ghcr.io/openshift-pipelines/tekton-wrap-pipeline/base:latest"
)

type ResolvedWrapperResource struct {
	Content     []byte
	PipelineRef string
}

var _ framework.ResolvedResource = &ResolvedWrapperResource{}

// Data returns the bytes of the file resolved from git.
func (r *ResolvedWrapperResource) Data() []byte {
	return r.Content
}

// Annotations returns the metadata that accompanies the resource fetched from the cluster.
func (r *ResolvedWrapperResource) Annotations() map[string]string {
	return map[string]string{
		"PipelineRef": r.PipelineRef,
	}
}

// Resolver implements a framework.Resolver that can "wrap" a Pipeline for not using a PVC for workspaces
type Resolver struct {
	kubeClientSet     kubernetes.Interface
	pipelineClientSet clientset.Interface
}

// Initialize sets up any dependencies needed by the Resolver. None atm.
func (r *Resolver) Initialize(ctx context.Context) error {
	r.kubeClientSet = client.Get(ctx)
	r.pipelineClientSet = pipelineclient.Get(ctx)
	return nil
}

// GetName returns a string name to refer to this Resolver by.
func (r *Resolver) GetName(context.Context) string {
	return "wrapresolver"
}

// GetConfigName returns the name of the wrap resolver's configmap.
func (r *Resolver) GetConfigName(context.Context) string {
	return "wrapresolver-config"
}

// GetSelector returns a map of labels to match requests to this Resolver.
func (r *Resolver) GetSelector(context.Context) map[string]string {
	return map[string]string{
		common.LabelKeyResolverType: LabelValueWrapResolverType,
	}
}

// ValidateParams ensures parameters from a request are as expected.
func (r *Resolver) ValidateParams(ctx context.Context, params map[string]string) error {
	_, err := populateParamsWithDefaults(ctx, params)
	return err
}

// Resolve uses the given params to resolve the requested file or resource.
func (r *Resolver) Resolve(ctx context.Context, origParams map[string]string) (framework.ResolvedResource, error) {
	logger := logging.FromContext(ctx)

	baseimage := DefaultBaseImage
	namespace := common.RequestNamespace(ctx)
	params, err := populateParamsWithDefaults(ctx, origParams)
	if err != nil {
		logger.Infof("wrap resolver parameter(s) invalid: %v", err)
		return nil, err
	}

	pipeline, err := r.pipelineClientSet.TektonV1beta1().Pipelines(namespace).Get(ctx, params[PipelineRefParam], metav1.GetOptions{})
	if err != nil {
		logger.Infof("failed to load pipeline %s from namespace %s: %v", params[PipelineRefParam], namespace, err)
		return nil, err
	}

	workspaces := sets.NewString(strings.Split(params[WorkspacesParam], ",")...)

	// Resolve tasks from Pipeline to embedded and mutate them
	taskSpecs, err := r.resolveTaskSpecs(ctx, &pipeline.Spec)
	if err != nil {
		logger.Infof("failed to resolve task specs from pipeline %s in namespace %s: %v", params[PipelineRefParam], namespace, err)
		return nil, err
	}

	newPipeline := pipeline.DeepCopy()
	wtargetimages := map[string]string{}
	for _, w := range workspaces.List() {
		wtargetimages[w] = strings.ReplaceAll(params[TargetParam], "{{workspace}}", w)
	}

	for i, t := range newPipeline.Spec.Tasks {
		taskWorkspaces := make([]string, len(t.Workspaces))
		for j, w := range t.Workspaces {
			taskWorkspaces[j] = w.Workspace
		}
		// Skip if not using the workspace
		if !workspaces.HasAny(taskWorkspaces...) {
			continue
		}

		s := taskSpecs[t.Name]
		// Except the first task, add a step to extract workspace content
		if i != 0 {
			var script strings.Builder
			fmt.Fprintf(&script, "#!/busybox/sh -e\n")
			for _, pw := range t.Workspaces {
				if workspaces.Has(pw.Workspace) {
					baseimage = wtargetimages[pw.Workspace]
					var w v1beta1.WorkspaceDeclaration
					for _, d := range s.Workspaces {
						if d.Name == pw.Name {
							w = d
						}
					}
					fmt.Fprintf(&script, `echo "Extract workspace content from %s in %s"
crane export %s | tar -x -C %s
`, baseimage, w.GetMountPath(), baseimage, w.GetMountPath())
				}
			}
			s.Steps = append([]v1beta1.Step{{
				Name:       "import-workspace",
				Image:      "gcr.io/go-containerregistry/crane:debug",
				WorkingDir: "/",
				Script:     script.String(),
			}}, s.Steps...)
		}

		var script strings.Builder
		fmt.Fprintf(&script, "#!/busybox/sh -e\n")
		for _, pw := range t.Workspaces {
			if workspaces.Has(pw.Workspace) {
				if i != 0 {
					baseimage = wtargetimages[pw.Workspace]
				}
				var w v1beta1.WorkspaceDeclaration
				for _, d := range s.Workspaces {
					if d.Name == pw.Name {
						w = d
					}
				}
				fmt.Fprintf(&script, `echo "Export workspace content from %s to %s"
(cd %s && tar -f - -c . | crane append -b %s -t %s -f -)
`, w.GetMountPath(), wtargetimages[pw.Workspace], w.GetMountPath(), baseimage, wtargetimages[pw.Workspace])
			}
		}
		s.Steps = append(s.Steps, v1beta1.Step{
			Name:       "export-workspace",
			Image:      "gcr.io/go-containerregistry/crane:debug",
			WorkingDir: "/",
			Script:     script.String(),
		})
		newPipeline.Spec.Tasks[i].TaskRef = nil
		newPipeline.Spec.Tasks[i].TaskSpec.TaskSpec = *s
	}

	newPipeline.Kind = "Pipeline"
	newPipeline.APIVersion = "tekton.dev/v1beta1"
	data, err := yaml.Marshal(newPipeline)
	if err != nil {
		logger.Infof("failed to marshal pipeline %s from namespace %s: %v", params[PipelineRefParam], namespace, err)
		return nil, err
	}

	return &ResolvedWrapperResource{
		Content:     data,
		PipelineRef: params[PipelineRefParam],
	}, nil
}

func (r *Resolver) resolveTaskSpecs(ctx context.Context, pipelineSpec *v1beta1.PipelineSpec) (map[string]*v1beta1.TaskSpec, error) {
	taskSpecs := map[string]*v1beta1.TaskSpec{}
	for _, t := range pipelineSpec.Tasks {
		var taskSpec *v1beta1.TaskSpec
		if t.TaskRef == nil {
			// Embedded TaskSpec, get it straight
			taskSpec = &t.TaskSpec.TaskSpec
		} else {
			var err error
			taskSpec, err = r.getTaskSpec(ctx, t.Name)
			if err != nil {
				return nil, fmt.Errorf("couldn't fetch taskspec for %s: %v", t.Name, err)
			}
		}
		taskSpecs[t.Name] = taskSpec
	}
	return taskSpecs, nil
}

func (r *Resolver) getTaskSpec(ctx context.Context, name string) (*v1beta1.TaskSpec, error) {
	namespace := common.RequestNamespace(ctx)
	t, err := r.pipelineClientSet.TektonV1beta1().Tasks(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	return &t.Spec, nil
}

func populateParamsWithDefaults(ctx context.Context, params map[string]string) (map[string]string, error) {
	conf := framework.GetResolverConfigFromContext(ctx)

	var missingParams []string

	if _, ok := params[WrapperParam]; !ok {
		if wrapperVal, ok := conf["default-wrapper"]; !ok {
			missingParams = append(missingParams, WrapperParam)
		} else {
			params[WrapperParam] = wrapperVal
		}
	}

	if _, ok := params[PipelineRefParam]; !ok {
		missingParams = append(missingParams, PipelineRefParam)
	}
	if _, ok := params[TargetParam]; !ok {
		missingParams = append(missingParams, TargetParam)
	}
	if _, ok := params[WorkspacesParam]; !ok {
		missingParams = append(missingParams, WorkspacesParam)
	}

	return params, nil
}
