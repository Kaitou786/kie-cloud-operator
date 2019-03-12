package defaults

//go:generate sh -c "CGO_ENABLED=0 go run .packr/packr.go $PWD"

import (
	"bytes"
	"context"
	"fmt"
	"html/template"
	"os"
	"regexp"
	"strings"

	"github.com/ghodss/yaml"
	"github.com/gobuffalo/packr"
	"github.com/imdario/mergo"
	v1 "github.com/kiegroup/kie-cloud-operator/pkg/apis/app/v1"
	"github.com/kiegroup/kie-cloud-operator/pkg/controller/kieapp/constants"
	"github.com/kiegroup/kie-cloud-operator/pkg/controller/kieapp/logs"
	"github.com/kiegroup/kie-cloud-operator/pkg/controller/kieapp/shared"
	"github.com/kiegroup/kie-cloud-operator/version"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

var log = logs.GetLogger("kieapp.defaults")

// GetEnvironment returns an Environment from merging the common config and the config
// related to the environment set in the KieApp definition
func GetEnvironment(cr *v1.KieApp, service v1.PlatformService) (v1.Environment, error) {
	envTemplate, err := getEnvTemplate(cr)
	if err != nil {
		return v1.Environment{}, err
	}
	var common v1.Environment
	yamlBytes, err := loadYaml(service, "common.yaml", cr.Namespace, envTemplate)
	if err != nil {
		return v1.Environment{}, err
	}
	err = yaml.Unmarshal(yamlBytes, &common)
	if err != nil {
		return v1.Environment{}, err
	}
	var env v1.Environment
	yamlBytes, err = loadYaml(service, fmt.Sprintf("envs/%s.yaml", cr.Spec.Environment), cr.Namespace, envTemplate)
	if err != nil {
		return v1.Environment{}, err
	}
	err = yaml.Unmarshal(yamlBytes, &env)
	if err != nil {
		return v1.Environment{}, err
	}

	mergedEnv, err := merge(common, env)
	if err != nil {
		return v1.Environment{}, err
	}
	return mergedEnv, nil
}

func getEnvTemplate(cr *v1.KieApp) (v1.EnvTemplate, error) {
	if cr.Spec.ImageRegistry == (v1.KieAppRegistry{}) {
		cr.Spec.ImageRegistry.Registry = logs.GetEnv("REGISTRY", constants.ImageRegistry) // default to red hat registry
		cr.Spec.ImageRegistry.Insecure = logs.GetBoolEnv("INSECURE")
	}

	// set default values for go template where not provided
	config := &cr.Spec.CommonConfig
	if config.ApplicationName == "" {
		config.ApplicationName = cr.Name
	}
	setAppConstants(&cr.Spec)
	isTrialEnv := strings.HasSuffix(string(cr.Spec.Environment), constants.TrialEnvSuffix)
	setPasswords(config, isTrialEnv)

	serversConfig, err := getServersConfig(cr, config)
	if err != nil {
		return v1.EnvTemplate{}, err
	}
	envTemplate := v1.EnvTemplate{
		CommonConfig: config,
		Console:      getConsoleTemplate(cr),
		Servers:      serversConfig,
		SmartRouter:  getSmartRouterTemplate(cr),
	}
	if err := configureAuth(cr, &envTemplate); err != nil {
		log.Error("unable to setup authentication: ", err)
		return envTemplate, err
	}

	return envTemplate, nil
}

func getConsoleTemplate(cr *v1.KieApp) v1.ConsoleTemplate {
	appConstants, hasEnv := constants.EnvironmentConstants[cr.Spec.Environment]
	template := v1.ConsoleTemplate{}
	if !hasEnv {
		return template
	}
	if cr.Spec.Objects.Console.KeystoreSecret == "" {
		template.KeystoreSecret = fmt.Sprintf(constants.KeystoreSecret, strings.Join([]string{cr.Spec.CommonConfig.ApplicationName, "businesscentral"}, "-"))
	} else {
		template.KeystoreSecret = cr.Spec.Objects.Console.KeystoreSecret
	}
	// Set replicas
	var denyScale bool
	if replicaConstants, hasReplicas := constants.ReplicaConstants[cr.Spec.Environment]; hasReplicas {
		template.Replicas, denyScale = setReplicas(cr.Spec.Objects.Console.KieAppObject, replicaConstants.Console, hasReplicas)
	} else {
		template.Replicas, denyScale = setReplicas(cr.Spec.Objects.Console.KieAppObject, v1.Replicas{}, false)
	}
	if denyScale {
		cr.Spec.Objects.Console.Replicas = Pint32(template.Replicas)
	}

	template.Name = appConstants.Prefix
	template.ImageName = appConstants.ImageName
	template.ProbePage = appConstants.ConsoleProbePage

	return template
}

func getSmartRouterTemplate(cr *v1.KieApp) v1.SmartRouterTemplate {
	template := v1.SmartRouterTemplate{}
	if cr.Spec.Objects.SmartRouter.KeystoreSecret == "" {
		template.KeystoreSecret = fmt.Sprintf(constants.KeystoreSecret, strings.Join([]string{cr.Spec.CommonConfig.ApplicationName, "smartrouter"}, "-"))
	} else {
		template.KeystoreSecret = cr.Spec.Objects.SmartRouter.KeystoreSecret
	}

	// Set replicas
	var denyScale bool
	if replicaConstants, hasReplicas := constants.ReplicaConstants[cr.Spec.Environment]; hasReplicas {
		template.Replicas, denyScale = setReplicas(cr.Spec.Objects.SmartRouter, replicaConstants.SmartRouter, hasReplicas)
	} else {
		template.Replicas, denyScale = setReplicas(cr.Spec.Objects.SmartRouter, v1.Replicas{}, false)
	}
	if denyScale {
		cr.Spec.Objects.SmartRouter.Replicas = Pint32(template.Replicas)
	}

	return template
}

func setReplicas(object v1.KieAppObject, replicaConstant v1.Replicas, hasReplicas bool) (replicas int32, denyScale bool) {
	if object.Replicas != nil {
		if replicaConstant.DenyScale && *object.Replicas != replicaConstant.Replicas {
			log.Warnf("scaling not allowed for this environment, setting to default of %d", replicaConstant.Replicas)
			return replicaConstant.Replicas, true
		}
		return *object.Replicas, false
	}
	if hasReplicas {
		return replicaConstant.Replicas, false
	}
	log.Warnf("no replicas settings for this environment, defaulting to %d", replicas)
	return int32(1), denyScale
}

// serverSortBlanks moves blank names to the end
func serverSortBlanks(serverSets []v1.KieServerSet) []v1.KieServerSet {
	var newSets []v1.KieServerSet
	// servers with existing names should be placed in front
	for index := range serverSets {
		if serverSets[index].Name != "" {
			newSets = append(newSets, serverSets[index])
		}
	}
	// servers without names should be at the end
	for index := range serverSets {
		if serverSets[index].Name == "" {
			newSets = append(newSets, serverSets[index])
		}
	}
	if len(newSets) != len(serverSets) {
		log.Error("slice lengths aren't equal, returning server sets w/o blank names sorted")
		return serverSets
	}
	return newSets
}

// Returns the templates to use depending on whether the spec was defined with a common configuration
// or a specific one.
func getServersConfig(cr *v1.KieApp, commonConfig *v1.CommonConfig) ([]v1.ServerTemplate, error) {
	var servers []v1.ServerTemplate
	if len(cr.Spec.Objects.Servers) == 0 {
		cr.Spec.Objects.Servers = []v1.KieServerSet{{}}
	}
	cr.Spec.Objects.Servers = serverSortBlanks(cr.Spec.Objects.Servers)
	usedNames := map[string]bool{}
	for index := range cr.Spec.Objects.Servers {
		serverSet := &cr.Spec.Objects.Servers[index]
		if serverSet.Name == "" {
			if len(cr.Spec.Objects.Servers) == 1 {
				serverSet.Name = fmt.Sprintf("%v-kieserver", cr.Spec.CommonConfig.ApplicationName)
			} else {
				serverSet.Name = fmt.Sprintf("%v-kieserver%v", cr.Spec.CommonConfig.ApplicationName, index)
			}
		} else if usedNames[serverSet.Name] {
			return []v1.ServerTemplate{}, fmt.Errorf("duplicate kieserver name %s", serverSet.Name)
		} else {
			usedNames[serverSet.Name] = true
		}
		if serverSet.Deployments == nil {
			serverSet.Deployments = Pint(constants.DefaultKieDeployments)
		}
		template := v1.ServerTemplate{
			Build:          getBuildConfig(commonConfig, serverSet.Build),
			KeystoreSecret: serverSet.KeystoreSecret,
		}
		if serverSet.Build != nil {
			if *serverSet.Deployments > 1 {
				return []v1.ServerTemplate{}, fmt.Errorf("Cannot request %v deployments for a build", *serverSet.Deployments)
			}
			template.From = corev1.ObjectReference{
				Kind:      "ImageStreamTag",
				Name:      fmt.Sprintf("%s-kieserver:latest", commonConfig.ApplicationName),
				Namespace: "",
			}
		} else {
			template.From = getDefaultKieServerImage(commonConfig, serverSet.From)
		}

		// Set replicas
		var denyScale bool
		if replicaConstants, hasReplicas := constants.ReplicaConstants[cr.Spec.Environment]; hasReplicas {
			template.Replicas, denyScale = setReplicas(serverSet.KieAppObject, replicaConstants.Server, hasReplicas)
		} else {
			template.Replicas, denyScale = setReplicas(serverSet.KieAppObject, v1.Replicas{}, false)
		}
		if denyScale {
			serverSet.Replicas = Pint32(template.Replicas)
		}

		template.KieName = serverSet.Name
		if *serverSet.Deployments == 1 {
			template.KieIndex = GetKieIndex(serverSet, 0)
			if template.KeystoreSecret == "" {
				template.KeystoreSecret = fmt.Sprintf(constants.KeystoreSecret, strings.Join([]string{template.KieName, template.KieIndex}, ""))
			}
			servers = append(servers, template)
		} else {
			for i := 0; i < *serverSet.Deployments; i++ {
				instanceTemplate := template.DeepCopy()
				instanceTemplate.KieIndex = GetKieIndex(serverSet, i)
				if instanceTemplate.KeystoreSecret == "" {
					instanceTemplate.KeystoreSecret = fmt.Sprintf(constants.KeystoreSecret, strings.Join([]string{instanceTemplate.KieName, instanceTemplate.KieIndex}, ""))
				}
				servers = append(servers, *instanceTemplate)
			}
		}
	}
	return servers, nil
}

// GetServerSet retrieves to correct ServerSet for processing
func GetServerSet(cr *v1.KieApp, requestedIndex int) (serverSet v1.KieServerSet, relativeIndex int) {
	count := 0
	for _, thisServerSet := range cr.Spec.Objects.Servers {
		for relativeIndex = 0; relativeIndex < *thisServerSet.Deployments; relativeIndex++ {
			if count == requestedIndex {
				serverSet = thisServerSet
				return
			}
			count++
		}
	}
	return
}

// ConsolidateObjects construct all CustomObjects prior to creation
func ConsolidateObjects(env v1.Environment, cr *v1.KieApp) v1.Environment {
	env.Console = ConstructObject(env.Console, cr.Spec.Objects.Console.KieAppObject)
	env.SmartRouter = ConstructObject(env.SmartRouter, cr.Spec.Objects.SmartRouter)
	for index := range env.Servers {
		serverSet, _ := GetServerSet(cr, index)
		env.Servers[index] = ConstructObject(env.Servers[index], serverSet.KieAppObject)
	}
	return env
}

// ConstructObject returns an object after merging the environment object and the one defined in the CR
func ConstructObject(object v1.CustomObject, appObject v1.KieAppObject) v1.CustomObject {
	for dcIndex, dc := range object.DeploymentConfigs {
		for containerIndex, c := range dc.Spec.Template.Spec.Containers {
			c.Env = shared.EnvOverride(c.Env, appObject.Env)
			err := mergo.Merge(&c.Resources, appObject.Resources, mergo.WithOverride)
			if err != nil {
				log.Error("Error merging interfaces. ", err)
			}
			dc.Spec.Template.Spec.Containers[containerIndex] = c
		}
		object.DeploymentConfigs[dcIndex] = dc
	}
	return object
}

// GetKieIndex aids in server indexing, depending on number of deployments and sets
func GetKieIndex(serverSet *v1.KieServerSet, relativeIndex int) string {
	if *serverSet.Deployments == 1 || relativeIndex == 0 {
		return ""
	}
	return fmt.Sprintf("%s%d", "-", relativeIndex+1)
}

func getBuildConfig(config *v1.CommonConfig, build *v1.KieAppBuildObject) v1.BuildTemplate {
	if build == nil {
		return v1.BuildTemplate{}
	}
	buildTemplate := v1.BuildTemplate{
		GitSource:                    build.GitSource,
		GitHubWebhookSecret:          getWebhookSecret(v1.GitHubWebhook, build.Webhooks),
		GenericWebhookSecret:         getWebhookSecret(v1.GenericWebhook, build.Webhooks),
		KieServerContainerDeployment: build.KieServerContainerDeployment,
		MavenMirrorURL:               build.MavenMirrorURL,
		ArtifactDir:                  build.ArtifactDir,
	}
	buildTemplate.From = getDefaultKieServerImage(config, build.From)
	return buildTemplate
}

func getDefaultKieServerImage(config *v1.CommonConfig, from *corev1.ObjectReference) corev1.ObjectReference {
	if from != nil {
		return *from
	}
	imageName := fmt.Sprintf("%s%s-kieserver-openshift:%s", config.Product, config.Version, constants.ImageStreamTag)
	return corev1.ObjectReference{
		Kind:      "ImageStreamTag",
		Name:      imageName,
		Namespace: constants.ImageStreamNamespace,
	}
}

func setPasswords(config *v1.CommonConfig, isTrialEnv bool) {
	passwords := []*string{
		&config.KeyStorePassword,
		&config.AdminPassword,
		&config.ControllerPassword,
		&config.MavenPassword,
		&config.ServerPassword}

	for i := range passwords {
		if len(*passwords[i]) != 0 {
			continue
		}
		if isTrialEnv {
			*passwords[i] = constants.DefaultPassword
		} else {
			*passwords[i] = string(shared.GeneratePassword(8))
		}
	}
}

func getWebhookSecret(webhookType v1.WebhookType, webhooks []v1.WebhookSecret) string {
	for _, webhook := range webhooks {
		if webhook.Type == webhookType {
			return webhook.Secret
		}
	}
	return string(shared.GeneratePassword(8))
}

// important to parse template first with this function, before unmarshalling into object
func loadYaml(service v1.PlatformService, filename, namespace string, env v1.EnvTemplate) ([]byte, error) {
	if _, _, useEmbedded := UseEmbeddedFiles(service); useEmbedded {
		box := packr.NewBox("../../../../config")
		if box.Has(filename) {
			yamlString, err := box.FindString(filename)
			if err != nil {
				return nil, err
			}
			return parseTemplate(env, yamlString), nil
		}
		return nil, fmt.Errorf("%s does not exist, '%s' KieApp not deployed", filename, env.ApplicationName)
	}

	cmName, file := convertToConfigMapName(filename)
	configMap := &corev1.ConfigMap{}
	err := service.Get(context.TODO(), types.NamespacedName{Name: cmName, Namespace: namespace}, configMap)
	if err != nil {
		return nil, fmt.Errorf("%s/%s ConfigMap not yet accessible, '%s' KieApp not deployed. Retrying... ", namespace, cmName, env.ApplicationName)
	}
	log.Debugf("Reconciling '%s' KieApp with %s from ConfigMap '%s'", env.ApplicationName, file, cmName)
	return parseTemplate(env, configMap.Data[file]), nil
}

func parseTemplate(env v1.EnvTemplate, objYaml string) []byte {
	var b bytes.Buffer

	tmpl, err := template.New(env.ApplicationName).Delims("[[", "]]").Parse(objYaml)
	if err != nil {
		log.Error("Error creating new Go template. ", err)
	}

	// template replacement
	err = tmpl.Execute(&b, env)
	if err != nil {
		log.Error("Error applying Go template. ", err)
	}

	return b.Bytes()
}

func convertToConfigMapName(filename string) (configMapName, file string) {
	name := constants.ConfigMapPrefix
	result := strings.Split(filename, "/")
	if len(result) > 1 {
		for i := 0; i < len(result)-1; i++ {
			name = strings.Join([]string{name, result[i]}, "-")
		}
	}
	return name, result[len(result)-1]
}

// ConfigMapsFromFile reads the files under the config folder and creates
// configmaps in the given namespace. It sets OwnerRef to operator deployment.
func ConfigMapsFromFile(myDep *appsv1.Deployment, ns string, scheme *runtime.Scheme) []corev1.ConfigMap {
	box := packr.NewBox("../../../../config")
	cmList := map[string][]map[string]string{}
	for _, filename := range box.List() {
		s, err := box.FindString(filename)
		if err != nil {
			log.Error("Error finding file with packr. ", err)
		}
		cmData := map[string]string{}
		cmName, file := convertToConfigMapName(filename)
		cmData[file] = s
		cmList[cmName] = append(cmList[cmName], cmData)
	}
	var configMaps []corev1.ConfigMap
	for cmName, dataSlice := range cmList {
		cmData := map[string]string{}
		for _, dataList := range dataSlice {
			for name, data := range dataList {
				cmData[name] = data
			}
		}
		cm := corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      cmName,
				Namespace: ns,
				Annotations: map[string]string{
					v1.SchemeGroupVersion.Group: version.Version,
				},
			},
			Data: cmData,
		}

		cm.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("ConfigMap"))
		err := controllerutil.SetControllerReference(myDep, &cm, scheme)
		if err != nil {
			log.Error("Error setting controller reference. ", err)
		}
		for index := range cm.OwnerReferences {
			cm.OwnerReferences[index].BlockOwnerDeletion = nil
		}
		configMaps = append(configMaps, cm)
	}
	return configMaps
}

// UseEmbeddedFiles checks environment variables WATCH_NAMESPACE & OPERATOR_NAME
func UseEmbeddedFiles(service v1.PlatformService) (opName string, depNameSpace string, useEmbedded bool) {
	namespace := os.Getenv(constants.NameSpaceEnv)
	name := os.Getenv(constants.OpNameEnv)
	if service.IsMockService() || namespace == "" || name == "" {
		return name, namespace, true
	}
	return name, namespace, false
}

// setAppConstants sets the application-related constants to use in the template processing
func setAppConstants(spec *v1.KieAppSpec) {
	env := spec.Environment
	appConstants, hasEnv := constants.EnvironmentConstants[env]
	if !hasEnv {
		return
	}
	if len(spec.CommonConfig.Version) == 0 {
		pattern := regexp.MustCompile("[0-9]+")
		spec.CommonConfig.Version = strings.Join(pattern.FindAllString(constants.ProductVersion, -1), "")
	}
	if len(spec.CommonConfig.ImageTag) == 0 {
		spec.CommonConfig.ImageTag = constants.ImageStreamTag
	}
	if len(spec.CommonConfig.Product) == 0 {
		spec.CommonConfig.Product = appConstants.Product
	}
	if len(spec.CommonConfig.MavenRepo) == 0 {
		spec.CommonConfig.MavenRepo = appConstants.MavenRepo
	}
}

// Pint returns a pointer to an integer
func Pint(i int) *int {
	return &i
}

// Pint32 returns a pointer to an integer
func Pint32(i int32) *int32 {
	return &i
}
