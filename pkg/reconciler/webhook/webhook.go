package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/openshift-pipelines/manual-approval-gate/pkg/apis/approvaltask/v1alpha1"
	"go.uber.org/zap"
	admissionv1 "k8s.io/api/admission/v1"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	admissionlisters "k8s.io/client-go/listers/admissionregistration/v1"
	corelisters "k8s.io/client-go/listers/core/v1"
	"knative.dev/pkg/controller"
	"knative.dev/pkg/kmp"
	"knative.dev/pkg/logging"
	"knative.dev/pkg/ptr"
	pkgreconciler "knative.dev/pkg/reconciler"
	"knative.dev/pkg/system"
	"knative.dev/pkg/webhook"
	certresources "knative.dev/pkg/webhook/certificates/resources"
)

const (
	Group   = "openshift-pipelines.org"
	Version = "v1alpha1"
	Kind    = "ApprovalTask"
)

// reconciler implements the AdmissionController for resources
type reconciler struct {
	webhook.StatelessAdmissionImpl
	pkgreconciler.LeaderAwareFuncs

	key  types.NamespacedName
	path string

	withContext func(context.Context) context.Context

	client       kubernetes.Interface
	vwhlister    admissionlisters.ValidatingWebhookConfigurationLister
	secretlister corelisters.SecretLister

	disallowUnknownFields bool
	secretName            string
}

var _ controller.Reconciler = (*reconciler)(nil)
var _ pkgreconciler.LeaderAware = (*reconciler)(nil)
var _ webhook.AdmissionController = (*reconciler)(nil)
var _ webhook.StatelessAdmissionController = (*reconciler)(nil)

// Reconcile implements controller.Reconciler
func (r *reconciler) Reconcile(ctx context.Context, key string) error {
	logger := logging.FromContext(ctx)

	if !r.IsLeaderFor(r.key) {
		return controller.NewSkipKey(key)
	}

	// Look up the webhook secret, and fetch the CA cert bundle.
	secret, err := r.secretlister.Secrets(system.Namespace()).Get(r.secretName)
	if err != nil {
		logger.Errorw("Error fetching secret", zap.Error(err))
		return err
	}

	caCert, ok := secret.Data[certresources.CACert]
	if !ok {
		return fmt.Errorf("secret %q is missing %q key", r.secretName, certresources.CACert)
	}

	// Reconcile the webhook configuration.
	return r.reconcileValidatingWebhook(ctx, caCert)
}

func (r *reconciler) Admit(ctx context.Context, request *admissionv1.AdmissionRequest) *admissionv1.AdmissionResponse {
	if r.withContext != nil {
		ctx = r.withContext(ctx)
	}

	logger := logging.FromContext(ctx)
	kind := request.Kind

	newBytes := request.Object.Raw
	gvk := schema.GroupVersionKind{
		Group:   kind.Group,
		Version: kind.Version,
		Kind:    kind.Kind,
	}

	if gvk.Group != Group || gvk.Version != Version || gvk.Kind != Kind {
		logger.Error("Unhandled kind: ", gvk)
	}

	oldBytes := request.OldObject.Raw
	var oldObj v1alpha1.ApprovalTask
	if len(oldBytes) != 0 {
		oldDecoder := json.NewDecoder(bytes.NewBuffer(oldBytes))
		if r.disallowUnknownFields {
			oldDecoder.DisallowUnknownFields()
		}
		if err := oldDecoder.Decode(&oldObj); err != nil {
			return webhook.MakeErrorStatus("cannot decode incoming old object: %v", err)
		}
	}

	// Check if approval is required by the approver
	if !isApprovalRequired(oldObj) {
		return &admissionv1.AdmissionResponse{
			Allowed: false,
			Result: &metav1.Status{
				Message: "ApprovalTask has already reached it's final state",
			},
		}
	}

	// Check if username is mentioned in the approval task
	if !ifUserExists(oldObj.Spec.Approvers, request) {
		return &admissionv1.AdmissionResponse{
			Allowed: false,
			Result: &metav1.Status{
				Message: "User does not exist in the approval list",
			},
		}
	}

	var newObj v1alpha1.ApprovalTask
	if len(newBytes) != 0 {
		newDecoder := json.NewDecoder(bytes.NewBuffer(newBytes))
		if r.disallowUnknownFields {
			newDecoder.DisallowUnknownFields()
		}
		if err := newDecoder.Decode(&newObj); err != nil {
			return webhook.MakeErrorStatus("cannot decode incoming new object: %v", err)
		}
	}

	// Check if user is updating the input for his name only
	var userApprovalChanged bool
	errMsg := fmt.Errorf("User can only update their own approval input")

	changed, err := IsUserApprovalChanged(oldObj.Spec.Approvers, newObj.Spec.Approvers, request)
	if err != nil {
		userApprovalChanged = false
		errMsg = fmt.Errorf("Invalid input change: %v", err)
	} else if changed {
		if CheckOtherUsersForInvalidChanges(oldObj.Spec.Approvers, newObj.Spec.Approvers, request) {
			userApprovalChanged = true
		} else {
			userApprovalChanged = false
		}
	} else {
		userApprovalChanged = false
	}

	if !userApprovalChanged {
		return &admissionv1.AdmissionResponse{
			Allowed: false,
			Result: &metav1.Status{
				Message: errMsg.Error(),
			},
		}
	}

	return &admissionv1.AdmissionResponse{
		Allowed: true,
	}
}

func (ac *reconciler) reconcileValidatingWebhook(ctx context.Context, caCert []byte) error {
	logger := logging.FromContext(ctx)
	rules := []admissionregistrationv1.RuleWithOperations{
		{
			Operations: []admissionregistrationv1.OperationType{
				admissionregistrationv1.Update,
				// admissionregistrationv1.Create,
			},
			Rule: admissionregistrationv1.Rule{
				APIGroups:   []string{"openshift-pipelines.org"},
				APIVersions: []string{"v1alpha1"},
				Resources:   []string{"approvaltask", "approvaltasks"},
			},
		},
	}

	configuredWebhook, err := ac.vwhlister.Get(ac.key.Name)
	if err != nil {
		return err
	}

	webhook := configuredWebhook.DeepCopy()

	webhook.OwnerReferences = nil

	for i, wh := range webhook.Webhooks {
		if wh.Name != webhook.Name {
			continue
		}
		webhook.Webhooks[i].Rules = rules
		webhook.Webhooks[i].ClientConfig.CABundle = caCert
		if webhook.Webhooks[i].ClientConfig.Service == nil {
			return fmt.Errorf("missing service reference for webhook: %s", wh.Name)
		}
		webhook.Webhooks[i].ClientConfig.Service.Path = ptr.String(ac.Path())
	}

	if ok, err := kmp.SafeEqual(configuredWebhook, webhook); err != nil {
		return fmt.Errorf("error diffing webhooks: %w", err)
	} else if !ok {
		logger.Info("Updating webhook")
		vwhclient := ac.client.AdmissionregistrationV1().ValidatingWebhookConfigurations()
		if _, err := vwhclient.Update(ctx, webhook, metav1.UpdateOptions{}); err != nil {
			return fmt.Errorf("failed to update webhook: %w", err)
		}
	} else {
		logger.Info("Webhook is valid")
	}
	return nil
}

// Path implements AdmissionController
func (ac *reconciler) Path() string {
	return ac.path
}

func ifUserExists(approvals []v1alpha1.ApproverDetails, request *admissionv1.AdmissionRequest) bool {
	if len(approvals) == 0 {
		return true
	}
	for _, approval := range approvals {
		switch v1alpha1.DefaultedApproverType(approval.Type) {
		case "User":
			if approval.Name == request.UserInfo.Username {
				return true
			}
		case "Group":
			// Check if user is in the group by checking the group name against user's groups
			for _, userGroup := range request.UserInfo.Groups {
				if approval.Name == userGroup {
					return true
				}
			}
			// Also check if user is explicitly listed in the group's users
			for _, user := range approval.Users {
				if user.Name == request.UserInfo.Username {
					return true
				}
			}
		}
	}
	return false
}

func isApprovalRequired(approvaltask v1alpha1.ApprovalTask) bool {
	if approvaltask.Status.State == "rejected" || approvaltask.Status.State == "approved" {
		return false
	}
	if len(approvaltask.Status.ApproversResponse) == approvaltask.Spec.NumberOfApprovalsRequired {
		return false
	}
	return true
}

// hasValidInputValue checks if the input value is either "approve" or "reject".
func hasValidInputValue(input string) error {
	if input == "approve" || input == "reject" {
		return nil
	}
	return fmt.Errorf("invalid input value: '%s'. Supported values are 'approve' or 'reject'", input)
}

// hasOnlyInputChanged checks if only the input field has changed for the current approver
// and if the new input value is valid
func hasOnlyInputChanged(oldObjApprover, newObjApprover v1alpha1.ApproverDetails) (bool, error) {
	if oldObjApprover.Name == newObjApprover.Name && oldObjApprover.Input != newObjApprover.Input {
		if err := hasValidInputValue(newObjApprover.Input); err != nil {
			return false, err
		}
		return true, nil
	}
	return false, nil
}

// IsUserApprovalChanged checks if there is a valid input change for the current user.
func IsUserApprovalChanged(oldObjApprovers, newObjApprovers []v1alpha1.ApproverDetails, request *admissionv1.AdmissionRequest) (bool, error) {
	currentUser := request.UserInfo.Username
	for i, approver := range oldObjApprovers {
		if approver.Name == currentUser && v1alpha1.DefaultedApproverType(approver.Type) == "User" {
			return hasOnlyInputChanged(approver, newObjApprovers[i])
		}

		if v1alpha1.DefaultedApproverType(approver.Type) == "Group" {
			// Check if current user is a member of this group
			isUserInGroup := false

			// Check if user is in the group by checking the group name against user's groups
			for _, userGroup := range request.UserInfo.Groups {
				if approver.Name == userGroup {
					isUserInGroup = true
					break
				}
			}

			// Also check if user is explicitly listed in the group's users
			for _, user := range approver.Users {
				if user.Name == currentUser {
					isUserInGroup = true
					break
				}
			}

			if isUserInGroup {
				// Allow changes to group-level input if user is in the group
				if i < len(newObjApprovers) {
					if approver.Input != newObjApprovers[i].Input {
						if err := hasValidInputValue(newObjApprovers[i].Input); err != nil {
							return false, err
						}
						return true, nil
					}
				}

				// Check if user is adding themselves to the group's users list
				oldUserFound := false
				newUserFound := false

				for _, user := range approver.Users {
					if user.Name == currentUser {
						oldUserFound = true
						break
					}
				}

				if i < len(newObjApprovers) {
					for _, user := range newObjApprovers[i].Users {
						if user.Name == currentUser {
							newUserFound = true
							break
						}
					}
				}

				// Allow user to add themselves to the group
				if !oldUserFound && newUserFound {
					// Validate the input they're setting for themselves
					if i < len(newObjApprovers) {
						for _, user := range newObjApprovers[i].Users {
							if user.Name == currentUser {
								if err := hasValidInputValue(user.Input); err != nil {
									return false, err
								}
								return true, nil
							}
						}
					}
					return true, nil
				}

				// Allow changes to individual user inputs within the group
				// Find current user in old users list
				var oldUserInput string
				userFoundInOld := false
				for _, user := range approver.Users {
					if user.Name == currentUser {
						oldUserInput = user.Input
						userFoundInOld = true
						break
					}
				}

				// Find current user in new users list
				var newUserInput string
				userFoundInNew := false
				if i < len(newObjApprovers) {
					for _, user := range newObjApprovers[i].Users {
						if user.Name == currentUser {
							newUserInput = user.Input
							userFoundInNew = true
							break
						}
					}
				}

				// Allow user to change their input if they're in both old and new lists
				if userFoundInOld && userFoundInNew && oldUserInput != newUserInput {
					if err := hasValidInputValue(newUserInput); err != nil {
						return false, err
					}
					return true, nil
				}
			}
		}
	}
	return false, nil
}

// CheckOtherUsersForInvalidChanges validates that no other approvers inputs have been changed
func CheckOtherUsersForInvalidChanges(oldObjApprovers, newObjApprover []v1alpha1.ApproverDetails, request *admissionv1.AdmissionRequest) bool {
	currentUser := request.UserInfo.Username
	for i, approver := range oldObjApprovers {
		if v1alpha1.DefaultedApproverType(approver.Type) == "User" && approver.Name != currentUser {
			if oldObjApprovers[i].Input != newObjApprover[i].Input {
				return false
			}
		}

		if v1alpha1.DefaultedApproverType(approver.Type) == "Group" {
			// Check if current user is a member of this group
			isUserInGroup := false

			// Check if user is in the group by checking the group name against user's groups
			for _, userGroup := range request.UserInfo.Groups {
				if approver.Name == userGroup {
					isUserInGroup = true
					break
				}
			}

			// Also check if user is explicitly listed in the group's users
			for _, user := range approver.Users {
				if user.Name == currentUser {
					isUserInGroup = true
					break
				}
			}

			// If current user is not in this group, they shouldn't be able to change the group-level input
			if !isUserInGroup {
				if i < len(newObjApprover) && approver.Input != newObjApprover[i].Input {
					return false
				}
			}

			// Check that only current user's input has changed in group users
			// Build maps of existing users for easier comparison
			oldUsers := make(map[string]string) // name -> input
			newUsers := make(map[string]string) // name -> input

			for _, user := range approver.Users {
				oldUsers[user.Name] = user.Input
			}

			if i < len(newObjApprover) {
				for _, user := range newObjApprover[i].Users {
					newUsers[user.Name] = user.Input
				}
			}

			// Check that existing users (other than current user) haven't changed their input
			for userName, oldInput := range oldUsers {
				if userName != currentUser {
					if newInput, exists := newUsers[userName]; exists {
						if oldInput != newInput {
							return false // Someone else's input changed
						}
					}
				}
			}

			// Check that no unauthorized users were added to the group
			for userName := range newUsers {
				if _, existedBefore := oldUsers[userName]; !existedBefore {
					// Someone new was added - only allow if it's the current user and they're a group member
					if userName != currentUser {
						return false // Someone other than current user was added
					}
					if !isUserInGroup {
						return false // Current user is not a member of this group
					}
				}
			}
		}
	}

	return true
}
