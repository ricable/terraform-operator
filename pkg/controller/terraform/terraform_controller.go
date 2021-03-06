package terraform

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/elliotchance/sshtunnel"
	"github.com/go-logr/logr"
	getter "github.com/hashicorp/go-getter"
	goSocks5 "github.com/isaaguilar/socks5-proxy"
	tfv1alpha1 "github.com/isaaguilar/terraform-operator/pkg/apis/tf/v1alpha1"
	"github.com/isaaguilar/terraform-operator/pkg/gitclient"
	"github.com/isaaguilar/terraform-operator/pkg/utils"
	giturl "github.com/whilp/git-urls"
	"golang.org/x/crypto/ssh"
	"golang.org/x/net/proxy"
	gitTransportClient "gopkg.in/src-d/go-git.v4/plumbing/transport/client"
	githttp "gopkg.in/src-d/go-git.v4/plumbing/transport/http"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	logf "sigs.k8s.io/controller-runtime/pkg/runtime/log"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

type ParsedAddress struct {
	sourcedir string
	subdirs   []string
	hash      string
	protocol  string
	uri       string
	host      string
	port      string
	user      string
	repo      string
}

type GitRepoAccessOptions struct {
	Client         gitclient.GitRepo
	Address        string
	Directory      string
	Extras         []string
	SCMAuthMethods []tfv1alpha1.SCMAuthMethod
	SSHProxy       tfv1alpha1.ProxyOpts
	tunnel         *sshtunnel.SSHTunnel
	ParsedAddress
}

type RunOptions struct {
	mainModule       string
	moduleConfigMaps []string
	namespace        string
	name             string
	tfvarsConfigMap  string
	envVars          map[string]string
	credentials      []tfv1alpha1.Credentials
	stack            ParsedAddress
	token            string
	tokenSecret      *tfv1alpha1.TokenSecretRef
	sshConfig        string
	sshConfigData    map[string][]byte
	applyAction      bool
	isNewResource    bool
	terraformVersion string
	serviceAccount   string
}

func newRunOptions(instance *tfv1alpha1.Terraform, isDestroy bool) RunOptions {
	// TODO Read the tfstate and decide IF_NEW_RESOURCE based on that
	isNewResource := false
	applyAction := false
	name := instance.Name
	terraformVersion := "0.11.14"

	if isDestroy {
		isNewResource = false
		applyAction = instance.Spec.Config.ApplyOnDelete
		name = name + "-destroy"
	} else if instance.ObjectMeta.Generation > 1 {
		isNewResource = false
		applyAction = instance.Spec.Config.ApplyOnUpdate
	} else {
		isNewResource = true
		applyAction = instance.Spec.Config.ApplyOnCreate
	}

	if instance.Spec.Stack.TerraformVersion != "" {
		terraformVersion = instance.Spec.Stack.TerraformVersion
	}

	return RunOptions{
		namespace:        instance.Namespace,
		name:             name,
		envVars:          make(map[string]string),
		isNewResource:    isNewResource,
		applyAction:      applyAction,
		terraformVersion: terraformVersion,
		serviceAccount:   "tf-" + name,
	}
}

func (r *RunOptions) updateDownloadedModules(module string) {
	r.moduleConfigMaps = append(r.moduleConfigMaps, module)
}

func (r *RunOptions) updateEnvVars(k, v string) {
	r.envVars[k] = v
}

const terraformFinalizer = "finalizer.tf.isaaguilar.com"

var _logf = logf.Log.WithName("controller_terraform")

/**
* USER ACTION REQUIRED: This is a scaffold file intended for the user to modify with their own Controller
* business logic.  Delete these comments after modifying this file.*
 */

// Add creates a new Terraform Controller and adds it to the Manager. The Manager will set fields on the Controller
// and Start it when the Manager is Started.
func Add(mgr manager.Manager) error {
	return add(mgr, newReconciler(mgr))
}

// newReconciler returns a new reconcile.Reconciler
func newReconciler(mgr manager.Manager) reconcile.Reconciler {
	return &ReconcileTerraform{
		client:   mgr.GetClient(),
		scheme:   mgr.GetScheme(),
		recorder: mgr.GetEventRecorderFor("terraform-controller"),
	}
}

// add adds a new Controller to mgr with r as the reconcile.Reconciler
func add(mgr manager.Manager, r reconcile.Reconciler) error {
	// Create a new controller
	c, err := controller.New("terraform-controller", mgr, controller.Options{Reconciler: r})
	if err != nil {
		return err
	}

	// Watch for changes to primary resource Terraform
	err = c.Watch(&source.Kind{Type: &tfv1alpha1.Terraform{}}, &handler.EnqueueRequestForObject{})
	if err != nil {
		return err
	}

	// Watch for terraform job completions
	err = c.Watch(&source.Kind{Type: &batchv1.Job{}}, &handler.EnqueueRequestForOwner{
		IsController: true,
		OwnerType:    &tfv1alpha1.Terraform{},
	})
	if err != nil {
		return err
	}

	// // Watch for changes to secondary resource Pods and requeue the owner Terraform
	// err = c.Watch(&source.Kind{Type: &corev1.Pod{}}, &handler.EnqueueRequestForOwner{
	// 	IsController: true,
	// 	OwnerType:    &tfv1alpha1.Terraform{},
	// })
	// if err != nil {
	// 	return err
	// }

	return nil
}

// blank assignment to verify that ReconcileTerraform implements reconcile.Reconciler
var _ reconcile.Reconciler = &ReconcileTerraform{}

// ReconcileTerraform reconciles a Terraform object
type ReconcileTerraform struct {
	// This client, initialized using mgr.Client() above, is a split client
	// that reads objects from the cache and writes to the apiserver
	client   client.Client
	scheme   *runtime.Scheme
	recorder record.EventRecorder
}

// func (r *ReconcileTerraform) manageError(obj metav1.Object, issue error) (reconcile.Result, error) {
// 	runtimeObj, ok := (obj).(runtime.Object)
// 	if !ok {
// 		return reconcile.Result{}, nil
// 	}
// 	var retryInterval time.Duration
// 	r.recorder.Event(runtimeObj, "Warning", "ProcessingError", issue.Error())

// 	return reconcile.Result{
// 		RequeueAfter: time.Duration(math.Min(float64(retryInterval.Nanoseconds()*2), float64(time.Hour.Nanoseconds()*6))),
// 		Requeue:      true,
// 	}, nil
// }

// Reconcile reads that state of the cluster for a Terraform object and makes changes based on the state read
// and what is in the Terraform.Spec
// TODO(user): Modify this Reconcile function to implement your Controller logic.  This example creates
// a Pod as an example
// Note:
// The Controller will requeue the Request to be processed again if the returned error is non-nil or
// Result.Requeue is true, otherwise upon completion it will remove the work from the queue.
func (r *ReconcileTerraform) Reconcile(request reconcile.Request) (reconcile.Result, error) {
	reqLogger := _logf.WithValues("Request.Namespace", request.Namespace, "Request.Name", request.Name)
	reqLogger.Info("Reconciling Terraform")

	// Fetch the Terraform instance
	instance := &tfv1alpha1.Terraform{}
	err := r.client.Get(context.TODO(), request.NamespacedName, instance)
	// reqLogger.Info(fmt.Sprintf("Here is the object's status before starting %+v", instance.Status))
	if err != nil {
		if errors.IsNotFound(err) {
			// Request object not found, could have been deleted after reconcile request.
			// Owned objects are automatically garbage collected. For additional cleanup logic use finalizers.
			// Return and don't requeue
			reqLogger.Info(fmt.Sprintf("Not found, instance is defined as: %+v", instance))
			reqLogger.Info("Terraform resource not found. Ignoring since object must be deleted")
			return reconcile.Result{}, nil
		}
		// Error reading the object - requeue the request.
		reqLogger.Error(err, "Failed to get Terraform")
		return reconcile.Result{}, err
	}

	// Check if the job is marked to be deleted, which is
	// indicated by the deletion timestamp being set.
	isMarkedToBeDeleted := instance.GetDeletionTimestamp() != nil
	if isMarkedToBeDeleted {
		if utils.ListContainsStr(instance.GetFinalizers(), terraformFinalizer) {
			// Run finalization logic for terraformFinalizer. If the
			// finalization logic fails, don't remove the finalizer so
			// that we can retry during the next reconciliation.

			// Completely ignore the finilization process if ignoreDelete is set
			if !instance.Spec.Config.IgnoreDelete {
				// let's make sure that a destroy job doesn't already exists
				d := types.NamespacedName{Namespace: request.Namespace, Name: request.Name + "-destroy"}
				destroyFound := &batchv1.Job{}
				err = r.client.Get(context.TODO(), d, destroyFound)
				if err != nil && errors.IsNotFound(err) {
					if err := r.setupAndRun(reqLogger, instance, true); err != nil {
						r.recorder.Event(instance, "Warning", "ProcessingError", err.Error())
						return reconcile.Result{}, err
					}
				} else if err != nil {
					reqLogger.Error(err, "Failed to get Job")
					r.recorder.Event(instance, "Warning", "ProcessingError", err.Error())
					return reconcile.Result{}, err
				}

				for {
					reqLogger.Info("Checking if destroy task is done")
					destroyFound = &batchv1.Job{}
					err = r.client.Get(context.TODO(), d, destroyFound)
					if err == nil {
						if destroyFound.Status.Succeeded > 0 {
							break
						}
						if destroyFound.Status.Failed > 6 {
							break
						}
					}
					time.Sleep(30 * time.Second)
				}
			}
			// Remove terraformFinalizer. Once all finalizers have been
			// removed, the object will be deleted.
			instance.SetFinalizers(utils.ListRemoveStr(instance.GetFinalizers(), terraformFinalizer))
			err := r.client.Update(context.TODO(), instance)
			if err != nil {
				r.recorder.Event(instance, "Warning", "ProcessingError", err.Error())
				return reconcile.Result{}, err
			}
		}
		return reconcile.Result{}, nil
	}

	if !utils.ListContainsStr(instance.GetFinalizers(), terraformFinalizer) {
		if !instance.Spec.Config.IgnoreDelete {
			if err := r.addFinalizer(reqLogger, instance); err != nil {
				r.recorder.Event(instance, "Warning", "ProcessingError", err.Error())
				return reconcile.Result{}, err
			}
		}
	}

	// Remove the finalizer when ignoreDelete exists. This is purley letting
	// the user see that there are no finalizers when get/describe the resource
	if instance.Spec.Config.IgnoreDelete && instance.ObjectMeta.Finalizers != nil {
		reqLogger.V(1).Info("Removing the finalizer since ignoreDelete is true")
		instance.SetFinalizers(utils.ListRemoveStr(instance.GetFinalizers(), terraformFinalizer))
		err := r.client.Update(context.TODO(), instance)
		if err != nil {
			r.recorder.Event(instance, "Warning", "ProcessingError", err.Error())
			return reconcile.Result{}, err
		}
	}

	// Check if the deployment already exists, if not create a new one
	found := &batchv1.Job{} // found gets updated in the next line
	err = r.client.Get(context.TODO(), request.NamespacedName, found)

	if err != nil && errors.IsNotFound(err) {
		err := r.setupAndRun(reqLogger, instance, false)
		if err != nil {
			return reconcile.Result{}, err
		}
		// Job created successfully - return and requeue
		return reconcile.Result{Requeue: true}, nil
	} else if err != nil {
		reqLogger.Error(err, "Failed to get Job")
		return reconcile.Result{}, err
	} else {
		// Found
		// reqLogger.Info(fmt.Sprintf("Job if found, printing status: %+v", found.Status))
	}

	if found.Status.Active != 0 {
		// The terraform is still being executed, wait until 0 active
		instance.Status.Phase = "running"
		r.client.Status().Update(context.TODO(), instance)
		requeueAfter := time.Duration(30 * time.Second)
		return reconcile.Result{Requeue: true, RequeueAfter: requeueAfter}, nil

	}

	if found.Status.Succeeded > 0 {

		// Check if job has already been stopped before and "generations" match.
		// The second predicate will be true when terraform spec is updated
		// after an already successful deployment.

		if instance.Status.Phase == "stopped" && instance.Status.LastGeneration != instance.ObjectMeta.Generation {
			// Delete the current job and restart
			reqLogger.V(1).Info("Preparing to restart job by first deleting old job")
			job := &batchv1.Job{}
			jobName := types.NamespacedName{Namespace: instance.Namespace, Name: instance.Name}
			err = r.client.Get(context.TODO(), jobName, job)
			if err != nil {
				return reconcile.Result{}, err
			}
			reqLogger.V(1).Info(fmt.Sprintf("Deleting the job: %+v", job.ObjectMeta))
			err = r.client.Delete(context.TODO(), job)
			// err = client.BatchV1().Jobs(instance.Namespace).Delete(instance.Name, &metav1.DeleteOptions{})
			if err != nil {
				return reconcile.Result{}, err
			}

			var timer int64 = 30 //seconds
			startDeleteTimer := time.Now().Unix()
			for {
				if (time.Now().Unix() - startDeleteTimer) > timer {
					return reconcile.Result{}, fmt.Errorf("Job could not delete in %d seconds", timer)
				}

				found := &batchv1.Job{}
				err = r.client.Get(context.TODO(), jobName, found)
				if err != nil && errors.IsNotFound(err) {
					reqLogger.V(1).Info("Old job deleted")
					return reconcile.Result{Requeue: true}, nil
				}
			}
		}
		now := time.Now()
		requeue := false
		instance.Status.Phase = "stopped"
		instance.Status.LastGeneration = instance.ObjectMeta.Generation
		r.client.Status().Update(context.TODO(), instance)

		// The terraform is still being executed, wait until 0 active
		cm, err := readConfigMap(r.client, instance.Name+"-status", instance.Namespace)
		if err != nil {
			return reconcile.Result{}, err
		}
		reqLogger.V(1).Info(fmt.Sprintf("Setting status of terraform plan as %v", cm.Data))

		// Find the successful pod
		collection := &corev1.PodList{}
		inNamespace := client.InNamespace(instance.Namespace)
		labelSelector := make(map[string]string)
		labelSelector["job-name"] = instance.Name
		matchingLabels := client.MatchingLabels(labelSelector)
		err = r.client.List(context.TODO(), collection, inNamespace, matchingLabels)
		if err != nil {
			return reconcile.Result{}, err
		}

		if len(collection.Items) == 0 {
			requeue = true
		}
		for _, pod := range collection.Items {
			// keep the pod around for 6 houra
			diff := now.Sub(pod.Status.StartTime.Time)
			if diff.Minutes() > 360 {
				_ = r.client.Delete(context.TODO(), &pod)
			}
		}

		requeueAfter := time.Duration(60 * time.Second)
		return reconcile.Result{Requeue: requeue, RequeueAfter: requeueAfter}, nil

	}

	// TODO should tf operator "auto" reconciliate (eg plan+apply)?
	// TODO manually triggers apply/destroy

	return reconcile.Result{}, nil
}

func (d GitRepoAccessOptions) TunnelClose() {
	if d.tunnel != nil {
		d.tunnel.Close()
	}
}

func formatJobSSHConfig(reqLogger logr.Logger, instance *tfv1alpha1.Terraform, k8sclient client.Client) (map[string][]byte, error) {
	data := make(map[string]string)
	dataAsByte := make(map[string][]byte)
	if instance.Spec.SSHProxy != nil {
		data["config"] = fmt.Sprintf("Host proxy\n"+
			"\tStrictHostKeyChecking no\n"+
			"\tUserKnownHostsFile=/dev/null\n"+
			"\tUser %s\n"+
			"\tHostname %s\n"+
			"\tIdentityFile ~/.ssh/proxy_key\n",
			instance.Spec.SSHProxy.User,
			instance.Spec.SSHProxy.Host)
		k := instance.Spec.SSHProxy.SSHKeySecretRef.Key
		if k == "" {
			k = "id_rsa"
		}
		ns := instance.Spec.SSHProxy.SSHKeySecretRef.Namespace
		if ns == "" {
			ns = instance.Namespace
		}

		key, err := loadPassword(k8sclient, k, instance.Spec.SSHProxy.SSHKeySecretRef.Name, ns)
		if err != nil {
			return dataAsByte, err
		}
		data["proxy_key"] = key

	}

	for _, m := range instance.Spec.SCMAuthMethods {

		// TODO validate SSH in resource manifest
		if m.Git.SSH != nil {
			if m.Git.SSH.RequireProxy {
				data["config"] += fmt.Sprintf("\nHost %s\n"+
					"\tStrictHostKeyChecking no\n"+
					"\tUserKnownHostsFile=/dev/null\n"+
					"\tHostname %s\n"+
					"\tIdentityFile ~/.ssh/%s\n"+
					"\tProxyJump proxy",
					m.Host,
					m.Host,
					m.Host)
			} else {
				data["config"] += fmt.Sprintf("\nHost %s\n"+
					"\tStrictHostKeyChecking no\n"+
					"\tUserKnownHostsFile=/dev/null\n"+
					"\tHostname %s\n"+
					"\tIdentityFile ~/.ssh/%s\n",
					m.Host,
					m.Host)
			}
			k := m.Git.SSH.SSHKeySecretRef.Key
			if k == "" {
				k = "id_rsa"
			}
			ns := m.Git.SSH.SSHKeySecretRef.Namespace
			if ns == "" {
				ns = instance.Namespace
			}
			key, err := loadPassword(k8sclient, k, m.Git.SSH.SSHKeySecretRef.Name, ns)
			if err != nil {
				return dataAsByte, err
			}
			data[m.Host] = key
		}
	}

	for k, v := range data {
		dataAsByte[k] = []byte(v)
	}

	return dataAsByte, nil
}

func (r *ReconcileTerraform) setupAndRun(reqLogger logr.Logger, instance *tfv1alpha1.Terraform, isFinalize bool) error {
	r.recorder.Event(instance, "Normal", "InitializeJobCreate", fmt.Sprintf("Setting up a Job"))
	// TODO(user): Add the cleanup steps that the operator
	// needs to do before the CR can be deleted. Examples
	// of finalizers include performing backups and deleting
	// resources that are not owned by this CR, like a PVC.

	runOpts := newRunOptions(instance, isFinalize)
	runOpts.updateEnvVars("DEPLOYMENT", instance.Name)
	// runOpts.namespace = instance.Namespace

	// Stack Download
	reqLogger.Info("Reading spec.stack config")
	if (tfv1alpha1.TerraformStack{}) == *instance.Spec.Stack {
		// This should never get reached since it violates the crd's
		// `spec.stack` requirement. Just in case, log an error.
		r.recorder.Event(instance, "Warning", "ConfigError", "No stack source defined")
		return fmt.Errorf("No stack source defined")
	} // else if (tfv1alpha1.SrcOpts{}) == *instance.Spec.Stack.Source {
	// 	return reconcile.Result{}, fmt.Errorf("No stack source defined")
	// }
	address := instance.Spec.Stack.Source.Address
	stackRepoAccessOptions, err := newGitRepoAccessOptionsFromSpec(instance, address, []string{})

	if err != nil {
		r.recorder.Event(instance, "Warning", "ProcessingError", fmt.Errorf("Error in newGitRepoAccessOptionsFromSpec: %v", err).Error())
		return fmt.Errorf("Error in newGitRepoAccessOptionsFromSpec: %v", err)
	}

	err = stackRepoAccessOptions.getParsedAddress()
	if err != nil {
		r.recorder.Event(instance, "Warning", "ProcessingError", fmt.Errorf("Error in parsing address: %v", err).Error())
		return fmt.Errorf("Error in parsing address: %v", err)
	}

	// Since we're not going to download this to a configmap, we need to
	// pass the information to the pod to do it. We should be able to
	// use stackRepoAccessOptions.parsedAddress and just send that to
	// the pod's environment vars.

	runOpts.updateDownloadedModules(stackRepoAccessOptions.hash)
	runOpts.stack = stackRepoAccessOptions.ParsedAddress

	// I think Terraform only allows for one git token. Add the first one
	// to the job's env vars as GIT_PASSWORD.
	for _, m := range stackRepoAccessOptions.SCMAuthMethods {
		if m.Git.HTTPS != nil {
			runOpts.tokenSecret = m.Git.HTTPS.TokenSecretRef
			if runOpts.tokenSecret.Key == "" {
				runOpts.tokenSecret.Key = "token"
			}
		}
		if m.Git.SSH != nil {
			sshConfigData, err := formatJobSSHConfig(reqLogger, instance, r.client)
			if err != nil {
				r.recorder.Event(instance, "Warning", "SSHConfigError", fmt.Errorf("%v", err).Error())
				return fmt.Errorf("Error setting up sshconfig: %v", err)
			}
			runOpts.sshConfigData = sshConfigData
		}
		break
	}

	runOpts.mainModule = stackRepoAccessOptions.hash
	//
	//
	// Download the tfvar configs (and optionally save to external repo)
	//
	//
	reqLogger.Info("Reading spec.config ")
	// TODO Validate spec.config exists
	// TODO validate spec.config.sources exists && len > 0
	runOpts.credentials = instance.Spec.Config.Credentails
	tfvars := ""
	otherConfigFiles := make(map[string]string)
	for _, s := range instance.Spec.Config.Sources {
		address := s.Address
		extras := s.Extras
		// Loop thru all the sources in spec.config
		configRepoAccessOptions, err := newGitRepoAccessOptionsFromSpec(instance, address, extras)
		if err != nil {
			r.recorder.Event(instance, "Warning", "ConfigError", fmt.Errorf("Error in Spec: %v", err).Error())
			return fmt.Errorf("Error in newGitRepoAccessOptionsFromSpec: %v", err)
		}
		err = configRepoAccessOptions.download(r.client, instance.Namespace)
		if err != nil {
			r.recorder.Event(instance, "Warning", "DownloadError", fmt.Errorf("Error in download: %v", err).Error())
			return fmt.Errorf("Error in download: %v", err)
		}
		defer configRepoAccessOptions.TunnelClose()

		reqLogger.V(1).Info(fmt.Sprintf("Config was downloaded and updated GitRepoAccessOptions: %+v", configRepoAccessOptions))

		tfvarSource, err := configRepoAccessOptions.tfvarFiles()
		if err != nil {
			r.recorder.Event(instance, "Warning", "ReadFileError", fmt.Errorf("Error reading tfvar files: %v", err).Error())
			return fmt.Errorf("Error in reading tfvarFiles: %v", err)
		}
		tfvars += tfvarSource

		otherConfigFiles, err = configRepoAccessOptions.otherConfigFiles()
		if err != nil {
			r.recorder.Event(instance, "Warning", "ReadFileError", fmt.Errorf("Error reading files: %v", err).Error())
			return fmt.Errorf("Error in reading otherConfigFiles: %v", err)
		}
	}
	data := make(map[string]string)
	data["tfvars"] = tfvars
	for k, v := range otherConfigFiles {
		data[k] = v
	}

	// Override the backend.tf by inserting a custom backend
	if instance.Spec.Config.CustomBackend != "" {
		data["backend_override.tf"] = instance.Spec.Config.CustomBackend
	}

	if instance.Spec.Config.PrerunScript != "" {
		data["prerun.sh"] = instance.Spec.Config.PrerunScript
	}

	// Do we need to run postrunscript's for finalizers?
	if instance.Spec.Config.PostrunScript != "" && !isFinalize {
		data["postrun.sh"] = instance.Spec.Config.PostrunScript
	}

	tfvarsConfigMap := instance.Name + "-tfvars"

	// make this a function for reuse
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      tfvarsConfigMap,
			Namespace: instance.Namespace,
		},
		Data: data,
	}
	controllerutil.SetControllerReference(instance, cm, r.scheme)
	err = r.client.Create(context.TODO(), cm)
	if err != nil && errors.IsNotFound(err) {
		r.recorder.Event(instance, "Warning", "ConfigMapCreateError", fmt.Errorf("Could not create configmap %v", err).Error())
		return err
	} else if err != nil {
		reqLogger.V(1).Info(fmt.Sprintf("ConfigMap %s will be updated", cm.Name))
		updateErr := r.client.Update(context.TODO(), cm)
		if updateErr != nil {
			r.recorder.Event(instance, "Warning", "ConfigMapUpdateError", fmt.Errorf("Could not update configmap %v", err).Error())
			return updateErr
		}
	}

	// err = c.createConfigMap(tfvarsConfigMap, instance.Namespace, make(map[string][]byte), data)
	// if err != nil {
	// 	r.recorder.Event(instance, "Warning", "ConfigMapCreateError", fmt.Errorf("Could not create configmap %v", err).Error())
	// 	return fmt.Errorf("Could not create configmap %v", err)
	// }
	runOpts.tfvarsConfigMap = tfvarsConfigMap

	// TODO Validate spec.config.env
	for _, env := range instance.Spec.Config.Env {
		runOpts.updateEnvVars(env.Name, env.Value)
	}

	// Flatten all the .tfvars and TF_VAR envs into a single file and push
	if instance.Spec.Config.ExportRepo != nil && !isFinalize {
		e := instance.Spec.Config.ExportRepo

		address := e.Address
		exportRepoAccessOptions, err := newGitRepoAccessOptionsFromSpec(instance, address, []string{})
		if err != nil {
			r.recorder.Event(instance, "Warning", "ConfigError", fmt.Errorf("Error getting git repo access options: %v", err).Error())
			return fmt.Errorf("Error in newGitRepoAccessOptionsFromSpec: %v", err)
		}
		// TODO decide what to do on errors
		// Closing the tunnel from within this function
		go exportRepoAccessOptions.commitTfvars(r.client, tfvars, e.TFVarsFile, e.ConfFile, instance.Namespace, instance.Spec.Config.CustomBackend, runOpts, reqLogger)
	}

	if isFinalize {
		runOpts.envVars["DESTROY"] = "true"
	}

	// RUN
	reqLogger.V(1).Info("Ready to run terraform")
	jobName, err := r.run(reqLogger, instance, runOpts)
	if err != nil {
		reqLogger.Error(err, "Failed to run job")
		r.recorder.Event(instance, "Warning", "StartJobError", err.Error())
		return err
	}

	r.recorder.Event(instance, "Normal", "SuccessfulCreate", fmt.Sprintf("Created Job: %s", jobName))

	if isFinalize {
		reqLogger.V(0).Info(fmt.Sprintf("Successfully finalized terraform on: %+v", instance))
	}

	return nil
}

func (r *ReconcileTerraform) addFinalizer(reqLogger logr.Logger, instance *tfv1alpha1.Terraform) error {
	reqLogger.Info("Adding Finalizer for terraform")
	instance.SetFinalizers(append(instance.GetFinalizers(), terraformFinalizer))

	// Update CR
	err := r.client.Update(context.TODO(), instance)
	if err != nil {
		reqLogger.Error(err, "Failed to update terraform with finalizer")
		return err
	}
	return nil
}

func (r RunOptions) generateServiceAccount() *corev1.ServiceAccount {
	annotations := make(map[string]string)

	for _, c := range r.credentials {
		if c.AWSCredentials.IRSA != "" {
			annotations["eks.amazonaws.com/role-arn"] = c.AWSCredentials.IRSA
		}
	}

	sa := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:        r.serviceAccount,
			Namespace:   r.namespace,
			Annotations: annotations,
		},
	}
	return sa
}

func (r RunOptions) generateActionConfigMap() *corev1.ConfigMap {
	data := make(map[string]string)

	if r.applyAction {
		data["action"] = "apply"
	} else {
		data["action"] = "plan-only"
	}

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      r.name + "-action",
			Namespace: r.namespace,
		},
		Data: data,
	}
	return cm
}

func (r RunOptions) generateRole() *rbacv1.Role {
	role := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{
			Name:      r.name,
			Namespace: r.namespace,
		},
		Rules: []rbacv1.PolicyRule{
			{
				Verbs:     []string{"*"},
				APIGroups: []string{""},
				Resources: []string{"configmaps"},
			},
		},
	}
	return role
}

func (r RunOptions) generateRoleBinding() *rbacv1.RoleBinding {
	rb := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      r.name,
			Namespace: r.namespace,
		},
		Subjects: []rbacv1.Subject{
			{
				Kind:      "ServiceAccount",
				Name:      r.serviceAccount,
				Namespace: r.namespace,
			},
		},
		RoleRef: rbacv1.RoleRef{
			Kind:     "Role",
			Name:     r.name,
			APIGroup: "rbac.authorization.k8s.io",
		},
	}
	return rb
}

func (r RunOptions) generateJob() *batchv1.Job {
	// reqLogger := log.WithValues("function", "run")
	// reqLogger.Info(fmt.Sprintf("Running job with this setup: %+v", r))

	// TF Module
	envs := []corev1.EnvVar{}
	if r.mainModule == "" {
		r.mainModule = "main_module"
	}
	envs = append(envs, []corev1.EnvVar{
		{
			Name:  "TFOPS_MAIN_MODULE",
			Value: r.mainModule,
		},
		{
			Name:  "NAMESPACE",
			Value: r.namespace,
		},
	}...)
	tfModules := []corev1.Volume{}
	// Check if stack is in a subdir
	if r.stack.repo != "" {
		envs = append(envs, []corev1.EnvVar{
			{
				Name:  "STACK_REPO",
				Value: r.stack.repo,
			},
			{
				Name:  "STACK_REPO_HASH",
				Value: r.stack.hash,
			},
		}...)
		if r.tokenSecret != nil {
			if r.tokenSecret.Name != "" {
				envs = append(envs, []corev1.EnvVar{
					{
						Name: "GIT_PASSWORD",
						ValueFrom: &corev1.EnvVarSource{
							SecretKeyRef: &corev1.SecretKeySelector{
								LocalObjectReference: corev1.LocalObjectReference{
									Name: r.tokenSecret.Name,
								},
								Key: r.tokenSecret.Key,
							},
						},
					},
				}...)
			}
		}

		// r.tokenSecret.Name
		// if r.token != "" {

		// }
		if len(r.stack.subdirs) > 0 {
			envs = append(envs, []corev1.EnvVar{
				{
					Name:  "STACK_REPO_SUBDIR",
					Value: r.stack.subdirs[0],
				},
			}...)
		}
	} else {
		for i, v := range r.moduleConfigMaps {
			tfModules = append(tfModules, []corev1.Volume{
				{
					Name: v,
					VolumeSource: corev1.VolumeSource{
						ConfigMap: &corev1.ConfigMapVolumeSource{
							LocalObjectReference: corev1.LocalObjectReference{
								Name: v,
							},
						},
					},
				},
			}...)

			envs = append(envs, []corev1.EnvVar{
				{
					Name:  "TFOPS_MODULE" + strconv.Itoa(i),
					Value: v,
				},
			}...)
		}
	}

	// Check if is new resource
	if r.isNewResource {
		envs = append(envs, []corev1.EnvVar{
			{
				Name:  "IS_NEW_RESOURCE",
				Value: "true",
			},
		}...)
	}

	// TF Vars
	for k, v := range r.envVars {
		envs = append(envs, []corev1.EnvVar{
			{
				Name:  k,
				Value: v,
			},
		}...)
	}
	tfVars := []corev1.Volume{}
	if r.tfvarsConfigMap != "" {
		tfVars = append(tfVars, []corev1.Volume{
			{
				Name: r.tfvarsConfigMap,
				VolumeSource: corev1.VolumeSource{
					ConfigMap: &corev1.ConfigMapVolumeSource{
						LocalObjectReference: corev1.LocalObjectReference{
							Name: r.tfvarsConfigMap,
						},
					},
				},
			},
		}...)

		envs = append(envs, []corev1.EnvVar{
			{
				Name:  "TFOPS_VARFILE_FLAG",
				Value: "-var-file /tfops/" + r.tfvarsConfigMap + "/tfvars",
			},
			{
				Name:  "TFOPS_CONFIGMAP_PATH",
				Value: "/tfops/" + r.tfvarsConfigMap,
			},
		}...)
	}
	volumes := append(tfModules, tfVars...)

	volumeMounts := []corev1.VolumeMount{}
	for _, v := range volumes {
		// setting up volumeMounts
		volumeMounts = append(volumeMounts, []corev1.VolumeMount{
			{
				Name:      v.Name,
				MountPath: "/tfops/" + v.Name,
			},
		}...)
	}

	if r.sshConfig != "" {
		mode := int32(0600)
		volumes = append(volumes, []corev1.Volume{
			{
				Name: "ssh-key",
				VolumeSource: corev1.VolumeSource{
					Secret: &corev1.SecretVolumeSource{
						SecretName:  r.sshConfig,
						DefaultMode: &mode,
					},
				},
			},
		}...)
		volumeMounts = append(volumeMounts, []corev1.VolumeMount{
			{
				Name:      "ssh-key",
				MountPath: "/root/.ssh/",
			},
		}...)
	}

	annotations := make(map[string]string)
	envFrom := []corev1.EnvFromSource{}

	for _, c := range r.credentials {
		if c.AWSCredentials.KIAM != "" {
			annotations["iam.amazonaws.com/role"] = c.AWSCredentials.KIAM
		}
	}

	for _, c := range r.credentials {
		if (tfv1alpha1.SecretNameRef{}) != c.SecretNameRef {
			envFrom = append(envFrom, []corev1.EnvFromSource{
				{
					SecretRef: &corev1.SecretEnvSource{
						LocalObjectReference: corev1.LocalObjectReference{
							Name: c.SecretNameRef.Name,
						},
					},
				},
			}...)
		}
	}

	// Schedule a job that will execute the terraform plan
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      r.name,
			Namespace: r.namespace,
		},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: annotations,
				},
				Spec: corev1.PodSpec{
					ServiceAccountName: r.serviceAccount,
					RestartPolicy:      "OnFailure",
					Containers: []corev1.Container{
						{
							Name: "tf",
							// TODO Version docker images more specifically than static versions
							Image:           "isaaguilar/tfops:" + r.terraformVersion,
							ImagePullPolicy: "Always",
							EnvFrom:         envFrom,
							Env: append(envs, []corev1.EnvVar{
								{
									Name:  "INSTANCE_NAME",
									Value: r.name,
								},
							}...),
							VolumeMounts: volumeMounts,
						},
					},
					Volumes: volumes,
				},
			},
		},
	}

	return job
}

func (r ReconcileTerraform) run(reqLogger logr.Logger, instance *tfv1alpha1.Terraform, runOpts RunOptions) (jobName string, err error) {
	runOpts.sshConfig = instance.Name + "-ssh-config"

	secret := generateSecretObject(runOpts.sshConfig, instance.Namespace, runOpts.sshConfigData)
	serviceAccount := runOpts.generateServiceAccount()
	roleBinding := runOpts.generateRoleBinding()
	role := runOpts.generateRole()
	configMap := runOpts.generateActionConfigMap()
	job := runOpts.generateJob()

	controllerutil.SetControllerReference(instance, secret, r.scheme)
	controllerutil.SetControllerReference(instance, serviceAccount, r.scheme)
	controllerutil.SetControllerReference(instance, roleBinding, r.scheme)
	controllerutil.SetControllerReference(instance, role, r.scheme)
	controllerutil.SetControllerReference(instance, configMap, r.scheme)
	controllerutil.SetControllerReference(instance, job, r.scheme)

	err = r.client.Create(context.TODO(), serviceAccount)
	if err != nil && errors.IsNotFound(err) {
		return "", err
	} else if err != nil {
		reqLogger.Info(err.Error())
	}

	err = r.client.Create(context.TODO(), role)
	if err != nil && errors.IsNotFound(err) {
		return "", err
	} else if err != nil {
		reqLogger.Info(err.Error())
	}

	err = r.client.Create(context.TODO(), roleBinding)
	if err != nil && errors.IsNotFound(err) {
		return "", err
	} else if err != nil {
		reqLogger.Info(err.Error())
	}

	err = r.client.Create(context.TODO(), configMap)
	if err != nil && errors.IsNotFound(err) {
		return "", err
	} else if err != nil {
		reqLogger.Info(fmt.Sprintf("ConfigMap %s already exists", configMap.Name))
		updateErr := r.client.Update(context.TODO(), configMap)
		if updateErr != nil && errors.IsNotFound(updateErr) {
			return "", updateErr
		} else if updateErr != nil {
			reqLogger.Info(err.Error())
		}
	}

	err = r.client.Create(context.TODO(), secret)
	if err != nil && errors.IsNotFound(err) {
		return "", err
	} else if err != nil {
		reqLogger.Info(fmt.Sprintf("Secret %s already exists", secret.Name))
		updateErr := r.client.Update(context.TODO(), secret)
		if updateErr != nil && errors.IsNotFound(updateErr) {
			return "", updateErr
		} else if updateErr != nil {
			reqLogger.Info(err.Error())
		}
	}

	err = r.client.Create(context.TODO(), job)
	if err != nil && errors.IsNotFound(err) {
		return "", err
	} else if err != nil {
		reqLogger.Info(err.Error())
	}

	return job.Name, nil
}

func newGitRepoAccessOptionsFromSpec(instance *tfv1alpha1.Terraform, address string, extras []string) (GitRepoAccessOptions, error) {
	d := GitRepoAccessOptions{}
	var sshProxyOptions tfv1alpha1.ProxyOpts

	// var tfAuthOptions []tfv1alpha1.AuthOpts

	// TODO allow configmaps as a source. This has to be parsed differently
	// before being passed to terraform's parsing mechanism

	temp, err := ioutil.TempDir("", "repo")
	if err != nil {
		return d, fmt.Errorf("Unable to make directory: %v", err)
	}
	// defer os.RemoveAll(temp) // clean up

	d = GitRepoAccessOptions{
		Address:   address,
		Extras:    extras,
		Directory: temp,
	}
	d.SCMAuthMethods = instance.Spec.SCMAuthMethods

	if instance.Spec.SSHProxy != nil {
		sshProxyOptions = *instance.Spec.SSHProxy
	}
	d.SSHProxy = sshProxyOptions

	return d, nil
}

func getHostKey(host string) (ssh.PublicKey, error) {
	file, err := os.Open(filepath.Join(os.Getenv("HOME"), ".ssh", "known_hosts"))
	if err != nil {
		return nil, err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	var hostKey ssh.PublicKey
	for scanner.Scan() {
		fields := strings.Split(scanner.Text(), " ")
		if len(fields) != 3 {
			continue
		}
		if strings.Contains(fields[0], host) {
			var err error
			hostKey, _, _, _, err = ssh.ParseAuthorizedKey(scanner.Bytes())
			if err != nil {
				return nil, fmt.Errorf("error parsing %q: %v", fields[2], err)
			}
			break
		}
	}

	if hostKey == nil {
		return nil, fmt.Errorf("no hostkey for %s", host)
	}
	return hostKey, nil
}

func (d GitRepoAccessOptions) tfvarFiles() (string, error) {
	// dump contents of tfvar files into a var
	tfvars := ""

	// TODO Should path definitions walk the path?
	if utils.ListContainsStr(d.Extras, "is-file") {
		for _, filename := range d.subdirs {
			if !strings.HasSuffix(filename, ".tfvars") {
				continue
			}
			file := filepath.Join(d.Directory, filename)
			content, err := ioutil.ReadFile(file)
			if err != nil {
				return "", fmt.Errorf("error reading file: %v", err)
			}
			tfvars += string(content) + "\n"
		}
	} else if len(d.subdirs) > 0 {
		for _, s := range d.subdirs {
			subdir := filepath.Join(d.Directory, s)
			lsdir, err := ioutil.ReadDir(subdir)
			if err != nil {
				return "", fmt.Errorf("error listing dir: %v", err)
			}

			for _, f := range lsdir {
				if strings.Contains(f.Name(), ".tfvars") {
					file := filepath.Join(subdir, f.Name())

					content, err := ioutil.ReadFile(file)
					if err != nil {
						return "", fmt.Errorf("error reading file: %v", err)
					}

					tfvars += string(content) + "\n"
				}
			}
		}
	} else {
		lsdir, err := ioutil.ReadDir(d.Directory)
		if err != nil {
			return "", fmt.Errorf("error listing dir: %v", err)
		}

		for _, f := range lsdir {
			if strings.Contains(f.Name(), ".tfvars") {
				file := filepath.Join(d.Directory, f.Name())

				content, err := ioutil.ReadFile(file)
				if err != nil {
					return "", fmt.Errorf("error reading file: %v", err)
				}

				tfvars += string(content) + "\n"
			}
		}
	}
	// TODO validate tfvars
	return tfvars, nil
}

// TODO combine this with the tfvars and make it a generic  get configs method
func (d GitRepoAccessOptions) otherConfigFiles() (map[string]string, error) {
	// create a configmap entry per source file
	configFiles := make(map[string]string)

	// TODO Should path definitions walk the path?
	if utils.ListContainsStr(d.Extras, "is-file") {
		for _, filename := range d.subdirs {
			file := filepath.Join(d.Directory, filename)
			content, err := ioutil.ReadFile(file)
			if err != nil {
				return configFiles, fmt.Errorf("error reading file: %v", err)
			}
			configFiles[filepath.Base(filename)] = string(content)
		}
	} else if len(d.subdirs) > 0 {
		for _, s := range d.subdirs {
			subdir := filepath.Join(d.Directory, s)
			lsdir, err := ioutil.ReadDir(subdir)
			if err != nil {
				return configFiles, fmt.Errorf("error listing dir: %v", err)
			}

			for _, f := range lsdir {

				file := filepath.Join(subdir, f.Name())

				content, err := ioutil.ReadFile(file)
				if err != nil {
					return configFiles, fmt.Errorf("error reading file: %v", err)
				}

				configFiles[f.Name()] = string(content)

			}
		}
	} else {
		lsdir, err := ioutil.ReadDir(d.Directory)
		if err != nil {
			return configFiles, fmt.Errorf("error listing dir: %v", err)
		}

		for _, f := range lsdir {

			file := filepath.Join(d.Directory, f.Name())

			content, err := ioutil.ReadFile(file)
			if err != nil {
				return configFiles, fmt.Errorf("error reading file: %v", err)
			}

			configFiles[f.Name()] = string(content)

		}
	}
	// TODO validate tfvars
	return configFiles, nil
}

// downloadFromSource will downlaod the files locally. It will also download
// tf modules locally if the user opts to. TF module downloading
// is probably going to be used in the event that go-getter cannot fetch the
// modules, perhaps becuase of a firewall. Check for proxy settings to send
// to the download command.
func downloadFromSource(src, moduleDir string) error {

	// Check for global proxy

	ds := getter.Detectors
	output, err := getter.Detect(src, moduleDir, ds)
	if err != nil {
		return fmt.Errorf("Could not Detect source: %v", err)
	}

	if strings.HasPrefix(output, "git::") {
		// send to gitSource
		return fmt.Errorf("There isn't an error, reading output as %v", output)
	} else if strings.HasPrefix(output, "https://") {
		return fmt.Errorf("downloadFromSource does not yet support http(s)")
	} else if strings.HasPrefix(output, "file://") {
		return fmt.Errorf("downloadFromSource does not yet support file")
	} else if strings.HasPrefix(output, "s3::") {
		return fmt.Errorf("downloadFromSource does not yet support s3")
	}

	// TODO If the total size of the stacks configmap is too large, it will have
	// to uploaded else where.

	return nil
}

func configureGitSSHString(user, host, port, uri string) string {
	if !strings.HasPrefix(uri, "/") {
		uri = "/" + uri
	}
	return fmt.Sprintf("ssh://%s@%s:%s%s", user, host, port, uri)
}

func tarBinaryData(fullpath, filename string) (map[string][]byte, error) {
	binaryData := make(map[string][]byte)
	// Archive the file and send to configmap
	// First remove the .git file if exists in Path
	gitFile := filepath.Join(fullpath, ".git")
	_, err := os.Stat(gitFile)
	if err == nil {
		if err = os.RemoveAll(gitFile); err != nil {
			return binaryData, fmt.Errorf("Could not find or remove .git: %v", err)
		}
	}

	tardir, err := ioutil.TempDir("", "tarball")
	if err != nil {
		return binaryData, fmt.Errorf("unable making tardir: %v", err)
	}
	defer os.RemoveAll(tardir) // clean up

	tarTarget := filepath.Join(tardir, "tarball")
	tarSource := filepath.Join(tardir, filename)

	err = os.Mkdir(tarTarget, 0755)
	if err != nil {
		return binaryData, fmt.Errorf("Could not create tarTarget: %v", err)
	}
	err = os.Mkdir(tarSource, 0755)
	if err != nil {
		return binaryData, fmt.Errorf("Could not create tarTarget: %v", err)
	}

	// expect result of untar to be same as filename. Copy src to a
	// "filename" dir instead of it's current dir
	// targetSrc := filepath.Join(target, fmt.Sprintf("%s", filename))
	err = utils.CopyDirectory(fullpath, tarSource)
	if err != nil {
		return binaryData, err
	}

	err = tarit("repo", tarSource, tarTarget)
	if err != nil {
		return binaryData, fmt.Errorf("error archiving '%s': %v", tarSource, err)
	}
	// files := make(map[string][]byte)
	tarballs, err := ioutil.ReadDir(tarTarget)
	if err != nil {
		return binaryData, fmt.Errorf("error listing tardir: %v", err)
	}
	for _, f := range tarballs {
		content, err := ioutil.ReadFile(filepath.Join(tarTarget, f.Name()))
		if err != nil {
			return binaryData, fmt.Errorf("error reading tarball: %v", err)
		}

		binaryData[f.Name()] = content
	}

	return binaryData, nil
}

func readConfigMap(k8sclient client.Client, name, namespace string) (*corev1.ConfigMap, error) {
	configMap := &corev1.ConfigMap{}
	namespacedName := types.NamespacedName{Namespace: namespace, Name: name}
	err := k8sclient.Get(context.TODO(), namespacedName, configMap)
	// configMap, err := c.clientset.CoreV1().ConfigMaps(namespace).Get(name, metav1.GetOptions{})
	if err != nil {
		return &corev1.ConfigMap{}, fmt.Errorf("error reading configmap: %v", err)
	}

	return configMap, nil
}

// func (c *k8sClient) createConfigMap(name, namespace string, binaryData map[string][]byte, data map[string]string) error {

// 	configMapObject := &corev1.ConfigMap{
// 		ObjectMeta: metav1.ObjectMeta{
// 			Name:      name,
// 			Namespace: namespace,
// 		},
// 		Data:       data,
// 		BinaryData: binaryData,
// 	}

// 	// TODO Make the terraform the referenced Owner of this resource
// 	_, err := c.clientset.CoreV1().ConfigMaps(namespace).Create(configMapObject)
// 	if err != nil {
// 		// fmt.Printf("The first create error... %v\n", err.Error())
// 		_, err = c.clientset.CoreV1().ConfigMaps(namespace).Update(configMapObject)
// 		if err != nil {
// 			return fmt.Errorf("error creating configmap: %v", err)
// 		}
// 	}

// 	return nil
// }

func generateSecretObject(name, namespace string, data map[string][]byte) *corev1.Secret {
	secretType := corev1.SecretType("opaque")
	secretObject := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Data: data,
		Type: secretType,
	}
	return secretObject
}

func loadPassword(k8sclient client.Client, key, name, namespace string) (string, error) {

	secret := &corev1.Secret{}
	namespacedName := types.NamespacedName{Namespace: namespace, Name: name}
	err := k8sclient.Get(context.TODO(), namespacedName, secret)
	// secret, err := c.clientset.CoreV1().Secrets(namespace).Get(name, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("Could not get secret: %v", err)
	}

	var password []byte
	for k, value := range secret.Data {
		if k == key {
			password = value
		}
	}

	if len(password) == 0 {
		return "", fmt.Errorf("unable to locate '%s' in secret: %v", key, err)
	}

	return string(password), nil

}

func loadPrivateKey(k8sclient client.Client, key, name, namespace string) (*os.File, error) {

	secret := &corev1.Secret{}
	namespacedName := types.NamespacedName{Namespace: namespace, Name: name}
	err := k8sclient.Get(context.TODO(), namespacedName, secret)
	if err != nil {
		return nil, fmt.Errorf("Could not get id_rsa secret: %v", err)
	}

	var privateKey []byte
	for k, value := range secret.Data {
		if k == key {
			privateKey = value
		}
	}

	if len(privateKey) == 0 {
		return nil, fmt.Errorf("unable to locate '%s' in secret: %v", key, err)
	}

	content := []byte(privateKey)
	tmpfile, err := ioutil.TempFile("", "id_rsa")
	if err != nil {
		return nil, fmt.Errorf("error creating tmpfile: %v", err)
	}

	if _, err := tmpfile.Write(content); err != nil {
		return nil, fmt.Errorf("unable to write tempfile: %v", err)
	}

	var mode os.FileMode
	mode = 0600
	os.Chmod(tmpfile.Name(), mode)

	return tmpfile, nil
}

func unique(s []string) []string {
	keys := make(map[string]bool)
	list := []string{}
	for _, entry := range s {
		if _, value := keys[entry]; !value {
			keys[entry] = true
			list = append(list, entry)
		}
	}
	return list
}

func tarit(filename, source, target string) error {
	reqLogger := _logf.WithValues("function", "tarit", "filename", filename)

	target = filepath.Join(target, fmt.Sprintf("%s.tar", filename))
	tarfile, err := os.Create(target)
	if err != nil {
		return err
	}
	defer tarfile.Close()

	tarball := tar.NewWriter(tarfile)
	defer tarball.Close()

	info, err := os.Stat(source)
	if err != nil {
		return nil
	}
	reqLogger.Info(fmt.Sprintf(""))

	var baseDir string
	if info.IsDir() {
		baseDir = filepath.Base(source)
	}

	return filepath.Walk(source,
		func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			header, err := tar.FileInfoHeader(info, info.Name())
			if err != nil {
				return err
			}

			if baseDir != "" {
				header.Name = filepath.Join(baseDir, strings.TrimPrefix(path, source))
			}

			if err := tarball.WriteHeader(header); err != nil {
				return err
			}

			if info.IsDir() {
				return nil
			}

			file, err := os.Open(path)
			if err != nil {
				return err
			}
			defer file.Close()
			_, err = io.Copy(tarball, file)
			return err
		})
}

func untar(tarball, target string) error {
	reader, err := os.Open(tarball)
	if err != nil {
		return err
	}
	defer reader.Close()
	tarReader := tar.NewReader(reader)

	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		} else if err != nil {
			return err
		}

		path := filepath.Join(target, header.Name)
		info := header.FileInfo()
		if info.IsDir() {
			if err = os.MkdirAll(path, info.Mode()); err != nil {
				return err
			}
			continue
		}

		file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, info.Mode())
		if err != nil {
			return err
		}
		defer file.Close()
		_, err = io.Copy(file, tarReader)
		if err != nil {
			return err
		}
	}
	return nil
}

func (d *GitRepoAccessOptions) download(k8sclient client.Client, namespace string) error {
	// This function only supports git modules. There's no explicit check
	// for this yet.
	// TODO document available options for sources
	reqLogger := _logf.WithValues("Download", d.Address, "Namespace", namespace, "Function", "download")
	reqLogger.Info("Starting download function")
	err := d.getParsedAddress()
	if err != nil {
		return fmt.Errorf("Error parsing address: %v", err)
	}
	repo := d.repo
	uri := d.uri

	if (tfv1alpha1.ProxyOpts{}) != d.SSHProxy {
		reqLogger.Info("Setting up a proxy")
		proxyAuthMethod, err := d.getProxyAuthMethod(k8sclient, namespace)
		if err != nil {
			return fmt.Errorf("Error getting proxyAuthMethod: %v", err)
		}

		if strings.Contains(d.protocol, "http") {
			proxyServer := ""
			if strings.Contains(d.host, ":") {
				proxyServer = d.SSHProxy.Host
			} else {
				fmt.Sprintf("%s:22", d.SSHProxy.Host)
			}

			hostKey := goSocks5.NewHostKey()
			duration := time.Duration(60 * time.Second)
			socks5Proxy := goSocks5.NewSocks5Proxy(hostKey, nil, duration)

			err := socks5Proxy.Start(d.SSHProxy.User, proxyServer, proxyAuthMethod)
			if err != nil {
				return fmt.Errorf("unable to start socks5: %v", err)
			}
			time.Sleep(100 * time.Millisecond)

			socks5Addr, err := socks5Proxy.Addr()
			if err != nil {
				return fmt.Errorf("unable to get socks5Addr: %v", err)
			}

			dialer, err := proxy.SOCKS5("tcp", socks5Addr, nil, proxy.Direct)
			if err != nil {
				return fmt.Errorf("unable to get dialer: %v", err)
			}

			httpTransport := &http.Transport{Dial: dialer.Dial}
			// set our socks5 as the dialer
			// httpTransport.Dial = dialer.Dial
			httpClient := &http.Client{Transport: httpTransport}

			gitTransportClient.InstallProtocol("http", githttp.NewClient(httpClient))
			gitTransportClient.InstallProtocol("https", githttp.NewClient(httpClient))
		} else if d.protocol == "ssh" {
			port, tunnel, err := d.setupSSHProxy(k8sclient, namespace)
			if err != nil {
				return err
			}

			if os.Getenv("DEBUG_SSHTUNNEL") != "" {
				tunnel.Log = log.New(os.Stdout, "", log.Ldate|log.Lmicroseconds)
			}
			d.tunnel = tunnel

			if strings.Index(uri, "/") != 0 {
				uri = "/" + uri
			}
			// configure auth with go git options
			repo = fmt.Sprintf("ssh://%s@127.0.0.1:%s%s", d.user, port, uri)
		}
	}
	reqLogger.Info(fmt.Sprintf("Getting ready to download source %s", repo))

	var gitRepo gitclient.GitRepo
	if d.protocol == "ssh" {
		filename, err := d.getGitSSHKey(k8sclient, namespace, d.protocol, reqLogger)
		if err != nil {
			return fmt.Errorf("Download failed for '%s': %v", repo, err)
		}
		defer os.Remove(filename)
		gitRepo, err = gitclient.GitSSHDownload(repo, d.Directory, filename, d.hash, reqLogger)
		if err != nil {
			return fmt.Errorf("Download failed for '%s': %v", repo, err)
		}
	} else {
		// TODO find out and support any other protocols
		// Just assume http is the only other protocol for now
		token, err := d.getGitToken(k8sclient, namespace, d.protocol, reqLogger)
		if err != nil {
			// Maybe we don't need to exit if no creds are used here
			reqLogger.Info(fmt.Sprintf("%v", err))
		}

		gitRepo, err = gitclient.GitHTTPDownload(repo, d.Directory, "git", token, d.hash)
		if err != nil {
			return fmt.Errorf("Download failed for '%s': %v", repo, err)
		}
	}

	// Set the hash and return
	d.Client = gitRepo
	d.hash, err = gitRepo.HashString()
	if err != nil {
		return err
	}
	reqLogger.Info(fmt.Sprintf("Hash: %v", d.hash))
	return nil
}

func (d *GitRepoAccessOptions) setupSSHProxy(k8sclient client.Client, namespace string) (string, *sshtunnel.SSHTunnel, error) {
	var port string
	var tunnel *sshtunnel.SSHTunnel
	proxyAuthMethod, err := d.getProxyAuthMethod(k8sclient, namespace)
	if err != nil {
		return port, tunnel, fmt.Errorf("Error getting proxyAuthMethod: %v", err)
	}
	proxyServerWithUser := fmt.Sprintf("%s@%s", d.SSHProxy.User, d.SSHProxy.Host)
	destination := ""
	if strings.Contains(d.host, ":") {
		destination = d.host
	} else {
		destination = fmt.Sprintf("%s:%s", d.host, d.port)
	}

	// Setup the tunnel, but do not yet start it yet.
	// // User and host of tunnel server, it will default to port 22
	// // if not specified.
	// proxyServerWithUser,

	// // Pick ONE of the following authentication methods:
	// // sshtunnel.PrivateKeyFile(filepath.Join(os.Getenv("HOME"), ".ssh", "id_rsa")), // 1. private key
	// proxyAuthMethod,

	// // The destination host and port of the actual server.
	// destination,
	tunnel = sshtunnel.NewSSHTunnel(proxyServerWithUser, proxyAuthMethod, destination, "0")

	// NewSSHTunnel will bind to a random port so that you can have
	// multiple SSH tunnels available. The port is available through:
	//   tunnel.Local.Port

	// You can use any normal Go code to connect to the destination server
	// through localhost. You may need to use 127.0.0.1 for some libraries.

	// You can provide a logger for debugging, or remove this line to
	// make it silent.
	// tunnel.Log = log.New(os.Stdout, "", log.Ldate|log.Lmicroseconds)
	// reqLogger.Info(tunnel.Log)

	// Start the server in the background. You will need to wait a
	// small amount of time for it to bind to the localhost port
	// before you can start sending connections.
	go tunnel.Start()
	time.Sleep(1000 * time.Millisecond)
	port = strconv.Itoa(tunnel.Local.Port)

	return port, tunnel, nil
}

func (d *GitRepoAccessOptions) getParsedAddress() error {
	sourcedir, subdirstr := getter.SourceDirSubdir(d.Address)
	// subdir can contain a list seperated by double slashes
	subdirs := strings.Split(subdirstr, "//")
	src := strings.TrimPrefix(sourcedir, "git::")
	var hash string
	if strings.Contains(sourcedir, "?") {
		for i, v := range strings.Split(sourcedir, "?") {
			if i > 0 {
				if strings.Contains(v, "&") {
					for _, w := range strings.Split(v, "&") {
						if strings.Contains(w, "ref=") {
							hash = strings.Split(w, "ref=")[1]
						}
					}

				} else if strings.Contains(v, "ref=") {
					hash = strings.Split(v, "ref=")[1]
				}
			}

		}
	}

	// strip out the url args
	repo := strings.Split(src, "?")[0]
	u, err := giturl.Parse(repo)
	if err != nil {
		return fmt.Errorf("unable to parse giturl: %v", err)
	}
	protocol := u.Scheme
	uri := strings.Split(u.RequestURI(), "?")[0]
	host := u.Host
	port := u.Port()
	if port == "" {
		if protocol == "ssh" {
			port = "22"
		} else if protocol == "https" {
			port = "443"
		}
	}

	user := u.User.Username()
	if user == "" {
		user = "git"
	}

	d.ParsedAddress = ParsedAddress{
		sourcedir: sourcedir,
		subdirs:   subdirs,
		hash:      hash,
		protocol:  protocol,
		uri:       uri,
		host:      host,
		port:      port,
		user:      user,
		repo:      repo,
	}
	return nil
}

func (d GitRepoAccessOptions) getProxyAuthMethod(k8sclient client.Client, namespace string) (ssh.AuthMethod, error) {
	var proxyAuthMethod ssh.AuthMethod

	name := d.SSHProxy.SSHKeySecretRef.Name
	key := d.SSHProxy.SSHKeySecretRef.Key
	if key == "" {
		key = "id_rsa"
	}
	ns := d.SSHProxy.SSHKeySecretRef.Namespace
	if ns == "" {
		ns = namespace
	}

	sshKey, err := loadPrivateKey(k8sclient, key, name, ns)
	if err != nil {
		return proxyAuthMethod, fmt.Errorf("unable to get privkey: %v", err)
	}
	defer os.Remove(sshKey.Name())
	defer sshKey.Close()
	proxyAuthMethod = sshtunnel.PrivateKeyFile(sshKey.Name())

	return proxyAuthMethod, nil
}

func (d *GitRepoAccessOptions) getGitSSHKey(k8sclient client.Client, namespace, protocol string, reqLogger logr.Logger) (string, error) {
	var filename string
	for _, m := range d.SCMAuthMethods {
		if m.Host == d.ParsedAddress.host && m.Git.SSH != nil {
			reqLogger.Info("Using Git over SSH with a key")
			name := m.Git.SSH.SSHKeySecretRef.Name
			key := m.Git.SSH.SSHKeySecretRef.Key
			if key == "" {
				key = "id_rsa"
			}
			ns := m.Git.SSH.SSHKeySecretRef.Namespace
			if ns == "" {
				ns = namespace
			}
			sshKey, err := loadPrivateKey(k8sclient, key, name, ns)
			if err != nil {
				return filename, err
			}
			defer sshKey.Close()
			filename = sshKey.Name()
		}
	}
	if filename == "" {
		return filename, fmt.Errorf("Failed to find Git SSH Key for %v\n", d.ParsedAddress.host)
	}
	return filename, nil
}

func (d *GitRepoAccessOptions) getGitToken(k8sclient client.Client, namespace, protocol string, reqLogger logr.Logger) (string, error) {
	var token string
	var err error
	for _, m := range d.SCMAuthMethods {
		if m.Host == d.ParsedAddress.host && m.Git.HTTPS != nil {
			reqLogger.Info("Using Git over HTTPS with a token")
			name := m.Git.HTTPS.TokenSecretRef.Name
			key := m.Git.HTTPS.TokenSecretRef.Key
			if key == "" {
				key = "token"
			}
			ns := m.Git.HTTPS.TokenSecretRef.Namespace
			if ns == "" {
				ns = namespace
			}
			token, err = loadPassword(k8sclient, key, name, ns)
			if err != nil {
				return token, fmt.Errorf("unable to get token: %v", err)
			}
		}
	}
	if token == "" {
		return token, fmt.Errorf("Failed to find Git token Key for %v\n", d.ParsedAddress.host)
	}
	return token, nil
}

func (d GitRepoAccessOptions) commitTfvars(k8sclient client.Client, tfvars, tfvarsFile, confFile, namespace, customBackend string, runOpts RunOptions, reqLogger logr.Logger) {
	filesToCommit := []string{}
	err := d.download(k8sclient, namespace)
	if err != nil {
		errString := fmt.Sprintf("Could not download repo %v", err)
		reqLogger.Info(errString)
		return
	}
	defer reqLogger.V(1).Info("exportRepoAccessOptions Closed connections")
	defer d.TunnelClose()
	defer reqLogger.V(1).Info("exportRepoAccessOptions Closing connections")
	// Create a file in the external repo
	err = d.Client.CheckoutBranch("")
	if err != nil {
		errString := fmt.Sprintf("Could not check out new branch %v", err)
		reqLogger.Info(errString)
		return
	}

	// Format TFVars File
	// Fist read in the tfvar file that gets created earlier. This tfvar
	// file should have already concatenated all the tfvars found
	// from the git repos
	tfvarsFileContent := tfvars
	for k, v := range runOpts.envVars {
		if !strings.Contains(k, "TF_VAR") {
			continue
		}
		k = strings.ReplaceAll(k, "TF_VAR_", "")
		if string(v[0]) != "{" && string(v[0]) != "[" {
			v = fmt.Sprintf("\"%s\"", v)
		}
		tfvarsFileContent = tfvarsFileContent + fmt.Sprintf("\n%s = %s", k, v)
	}

	// Remove Duplicates
	// TODO replace this code with a more terraform native method of merging tfvars
	var c bytes.Buffer
	var currentKey string
	var currentValue string
	keyIndexer := make(map[string]string)
	var openBrackets int
	for _, line := range strings.Split(tfvarsFileContent, "\n") {
		lineArr := strings.Split(line, "=")
		// ignore blank lines
		if strings.TrimSpace(lineArr[0]) == "" {
			continue
		}

		if openBrackets > 0 {
			currentValue += "\n" + strings.ReplaceAll(line, "\t", "  ")
			// Check for more open brackets and close brackets
			trimmedLine := strings.TrimSpace(line)
			lastCharIdx := len(trimmedLine) - 1
			lastChar := string(trimmedLine[lastCharIdx])
			lastTwoChar := ""
			if lastCharIdx > 0 {
				lastTwoChar = string(trimmedLine[lastCharIdx-1:])
			}

			if lastChar == "{" || lastChar == "[" {
				openBrackets++
			} else if lastChar == "}" || lastChar == "]" || lastTwoChar == "}," || lastTwoChar == "]," {
				openBrackets--
			}
			if openBrackets == 0 {
				keyIndexer[currentKey] = currentValue
			}
			continue
		}
		currentKey = strings.TrimSpace(lineArr[0])

		if len(lineArr) > 1 {
			lastLineArrIdx := len(lineArr) - 1
			trimmedLine := lineArr[lastLineArrIdx]
			lastCharIdx := len(trimmedLine) - 1
			lastChar := string(trimmedLine[lastCharIdx])
			if lastChar == "{" || lastChar == "[" {
				openBrackets++
			}
		} else {
			errString := fmt.Sprintf("Error in parsing tfvars string: %s", line)
			reqLogger.Info(errString)
			return
		}

		currentValue = line
		if openBrackets > 0 {
			continue
		}
		keyIndexer[currentKey] = currentValue
	}

	keys := make([]string, 0, len(keyIndexer))
	for k := range keyIndexer {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&c, "%s\n\n", keyIndexer[k])

	}

	// Write HCL to file
	// Create the path if not exists
	err = os.MkdirAll(filepath.Dir(filepath.Join(d.Directory, tfvarsFile)), 0755)
	if err != nil {
		errString := fmt.Sprintf("Could not create path: %v", err)
		reqLogger.Info(errString)
		return
	}
	err = ioutil.WriteFile(filepath.Join(d.Directory, tfvarsFile), c.Bytes(), 0644)
	if err != nil {
		errString := fmt.Sprintf("Could not write file %v", err)
		reqLogger.Info(errString)
		return
	}

	// Write to file

	filesToCommit = append(filesToCommit, tfvarsFile)

	// Format Conf File
	if confFile != "" {
		confFileContent := ""
		// The backend-configs for tf-operator are actually written
		// as a complete tf resource. We need to extract only the key
		// and values from the conf file only.
		if customBackend != "" {

			configsOnly := strings.Split(customBackend, "\n")
			for _, line := range configsOnly {
				// Assuming that config lines contain an equal sign
				// All other lines are discarded
				if strings.Contains(line, "=") {
					if confFileContent == "" {
						confFileContent = strings.TrimSpace(line)
					} else {
						confFileContent = confFileContent + "\n" + strings.TrimSpace(line)
					}
				}
			}
		}

		// Write to file
		err = os.MkdirAll(filepath.Dir(filepath.Join(d.Directory, confFile)), 0755)
		if err != nil {
			errString := fmt.Sprintf("Could not create path: %v", err)
			reqLogger.Info(errString)
			return
		}
		err = ioutil.WriteFile(filepath.Join(d.Directory, confFile), []byte(confFileContent), 0644)
		if err != nil {
			errString := fmt.Sprintf("Could not write file %v", err)
			reqLogger.Info(errString)
			return
		}
		filesToCommit = append(filesToCommit, confFile)
	}

	// Commit and push to repo
	commitMsg := fmt.Sprintf("automatic update via terraform-operator\nupdates to:\n%s", strings.Join(filesToCommit, "\n"))
	err = d.Client.Commit(filesToCommit, commitMsg)
	if err != nil {
		errString := fmt.Sprintf("Could not commit to repo %v", err)
		reqLogger.V(1).Info(errString)
		return
	}
	err = d.Client.Push("refs/heads/master")
	if err != nil {
		errString := fmt.Sprintf("Could not push to repo %v", err)
		reqLogger.V(1).Info(errString)
		return
	}

}
