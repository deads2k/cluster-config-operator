package options

import (
	"bytes"
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"text/template"

	"github.com/ghodss/yaml"
	configv1 "github.com/openshift/api/config/v1"
	"github.com/openshift/library-go/pkg/assets"
	"github.com/openshift/library-go/pkg/operator/configobserver/featuregates"
	"github.com/openshift/library-go/pkg/operator/resource/resourcemerge"
	"github.com/spf13/pflag"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// GenericOptions contains the generic render command options.
type GenericOptions struct {
	DefaultFile                    string
	BootstrapOverrideFile          string
	AdditionalConfigOverrideFiles  []string
	RenderedManifestInputFilenames []string

	ConfigOutputFile string

	TemplatesDir   string
	AssetInputDir  string
	AssetOutputDir string

	FeatureSet     string
	PayloadVersion string
}

type Template struct {
	FileName string
	Content  []byte
}

// NewGenericOptions returns a default set of generic options.
func NewGenericOptions() *GenericOptions {
	return &GenericOptions{
		TemplatesDir: "/usr/share/bootkube/manifests",
	}
}

// AddFlags adds the generic flags to the flagset.
func (o *GenericOptions) AddFlags(fs *pflag.FlagSet, configGVK schema.GroupVersionKind) {
	fs.StringVar(&o.AssetOutputDir, "asset-output-dir", o.AssetOutputDir, "Output path for rendered manifests.")
	fs.StringVar(&o.AssetInputDir, "asset-input-dir", o.AssetInputDir, "A path to directory with certificates and secrets.")
	fs.StringVar(&o.TemplatesDir, "templates-input-dir", o.TemplatesDir, "A path to a directory with manifest templates.")
	fs.StringSliceVar(&o.AdditionalConfigOverrideFiles, "config-override-files", o.AdditionalConfigOverrideFiles,
		fmt.Sprintf("Additional sparse %s files for customiziation through the installer, merged into the default config in the given order.", gvkOutput{configGVK}))
	fs.StringVar(&o.ConfigOutputFile, "config-output-file", o.ConfigOutputFile, fmt.Sprintf("Output path for the %s yaml file.", gvkOutput{configGVK}))
	fs.StringVar(&o.FeatureSet, "feature-set", o.FeatureSet, "Enables features that are not part of the default feature set.")
	fs.StringSliceVar(&o.RenderedManifestInputFilenames, "rendered-manifest-files", o.RenderedManifestInputFilenames,
		"files or directories containing yaml or json manifests that will be created via cluster-bootstrapping.")
	fs.StringVar(&o.PayloadVersion, "payload-version", o.PayloadVersion, "Version that will eventually be placed into ClusterOperator.status.  This normally comes from the CVO set via env var: OPERATOR_IMAGE_VERSION.")

}

type gvkOutput struct {
	schema.GroupVersionKind
}

func (gvk gvkOutput) String() string {
	return fmt.Sprintf("%s.%s/%s", gvk.GroupVersionKind.Kind, gvk.GroupVersionKind.Group, gvk.GroupVersionKind.Version)
}

// Complete fills in missing values before execution.
func (o *GenericOptions) Complete() error {
	return nil
}

// Validate verifies the inputs.
func (o *GenericOptions) Validate() error {
	if len(o.AssetInputDir) == 0 {
		return errors.New("missing required flag: --asset-input-dir")
	}
	if len(o.AssetOutputDir) == 0 {
		return errors.New("missing required flag: --asset-output-dir")
	}
	if len(o.TemplatesDir) == 0 {
		return errors.New("missing required flag: --templates-dir")
	}
	if len(o.ConfigOutputFile) == 0 {
		return errors.New("missing required flag: --config-output-file")
	}

	for _, filename := range o.RenderedManifestInputFilenames {
		_, err := os.Stat(filename)
		if err != nil {
			return fmt.Errorf("--rendered-manifest-files, value %q could not be read: %v", filename, err)
		}
	}

	switch configv1.FeatureSet(o.FeatureSet) {
	case configv1.Default, configv1.TechPreviewNoUpgrade, configv1.CustomNoUpgrade, configv1.LatencySensitive:
	default:
		return fmt.Errorf("invalid feature-set specified: %q", o.FeatureSet)
	}
	return nil
}

func (o *GenericOptions) ReadInputManifests() (RenderedManifests, error) {
	ret := RenderedManifests{}
	for _, filename := range o.RenderedManifestInputFilenames {
		manifestContent, err := assets.LoadFilesRecursively(filename)
		if err != nil {
			return nil, fmt.Errorf("failed loading rendered manifest inputs from %q: %w", filename, err)
		}
		for manifestFile, content := range manifestContent {
			ret = append(ret, RenderedManifest{
				OriginalFilename: manifestFile,
				Content:          content,
			})
		}
	}

	return ret, nil
}

func (o *GenericOptions) FeatureGates() (featuregates.FeatureGateAccess, error) {
	if len(o.PayloadVersion) == 0 {
		return nil, fmt.Errorf("cannot return FeatureGate without payload version")
	}
	if len(o.RenderedManifestInputFilenames) == 0 {
		return nil, fmt.Errorf("cannot return FeatureGate without rendered manifests")
	}

	inputManifest, err := o.ReadInputManifests()
	if err != nil {
		return nil, fmt.Errorf("error reading input manifests: %w", err)
	}
	featureGates := inputManifest.ListManifestOfType(configv1.GroupVersion.WithKind("FeatureGate"))
	if len(featureGates) == 0 {
		return nil, fmt.Errorf("no FeatureGates found in manfest dir: %v", o.RenderedManifestInputFilenames)
	}
	var prev *RenderedManifest
	var featureGate *configv1.FeatureGate
	for i := range featureGates {
		curr := featureGates[i]
		decodedObj, err := curr.GetDecodedObj()
		if err != nil {
			return nil, fmt.Errorf("decoding failure for %q: %w", curr.OriginalFilename, err)
		}
		currFeatureGate, ok := decodedObj.(*configv1.FeatureGate)
		if !ok {
			return nil, fmt.Errorf("wrong obj type for %q: %T: %v", curr.OriginalFilename, decodedObj, curr.Content)
		}
		if featureGate == nil {
			prev = &curr
			featureGate = currFeatureGate
			continue
		}

		if !equality.Semantic.DeepEqual(featureGate, currFeatureGate) {
			return nil, fmt.Errorf("FeatureGate manifests disagree: %q and %q, with \n%v\n%v ", prev.OriginalFilename, curr.OriginalFilename, prev.Content, curr.Content)
		}
	}

	ret, err := featuregates.NewHardcodedFeatureGateAccessFromFeatureGate(featureGate, o.PayloadVersion)
	if err != nil {
		return nil, fmt.Errorf("error creating feature accessor: %w", err)
	}

	return ret, nil
}

// ApplyTo applies the options to the given config struct using the provided text/template data.
func (o *GenericOptions) ApplyTo(cfg *FileConfig, defaultConfig, bootstrapOverrides Template, templateData interface{}, specialCases map[string]resourcemerge.MergeFunc) error {
	var err error

	cfg.BootstrapConfig, err = o.configFromDefaultsPlusOverride(defaultConfig, bootstrapOverrides, templateData, specialCases)
	if err != nil {
		return fmt.Errorf("failed to generate bootstrap config (phase 1): %v", err)
	}

	// load and render templates
	if cfg.Assets, err = assets.LoadFilesRecursively(o.AssetInputDir); err != nil {
		return fmt.Errorf("failed loading assets from %q: %v", o.AssetInputDir, err)
	}

	return nil
}

func (o *GenericOptions) configFromDefaultsPlusOverride(defaultConfig, overrides Template, templateData interface{}, specialCases map[string]resourcemerge.MergeFunc) ([]byte, error) {
	defaultConfigContent, err := renderTemplate(defaultConfig, templateData)
	if err != nil {
		return nil, fmt.Errorf("failed to render default config file %q as text/template: %v", defaultConfig.FileName, err)
	}

	overridesContent, err := renderTemplate(overrides, templateData)
	if err != nil {
		return nil, fmt.Errorf("failed to render config override file %q as text/template: %v", overrides.FileName, err)
	}
	configs := [][]byte{defaultConfigContent, overridesContent}
	for _, fname := range o.AdditionalConfigOverrideFiles {
		bs, err := ioutil.ReadFile(fname)
		if err != nil {
			return nil, fmt.Errorf("failed to load config overrides at %q: %v", fname, err)
		}
		overrides, err := renderTemplate(Template{fname, bs}, templateData)
		if err != nil {
			return nil, fmt.Errorf("failed to render config overrides file %q as text/template: %v", fname, err)
		}

		configs = append(configs, overrides)
	}
	mergedConfig, err := resourcemerge.MergeProcessConfig(specialCases, configs...)
	if err != nil {
		return nil, fmt.Errorf("failed to merge configs: %v", err)
	}
	yml, err := yaml.JSONToYAML(mergedConfig)
	if err != nil {
		return nil, err
	}

	return yml, nil
}

func renderTemplate(tpl Template, data interface{}) ([]byte, error) {
	tmpl, err := template.New(tpl.FileName).Parse(string(tpl.Content))
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}
