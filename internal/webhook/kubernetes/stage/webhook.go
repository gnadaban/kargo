package stage

import (
	"context"
	"errors"
	"fmt"

	admissionv1 "k8s.io/api/admission/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	kargoapi "github.com/akuity/kargo/api/v1alpha1"
	"github.com/akuity/kargo/internal/api"
	libWebhook "github.com/akuity/kargo/internal/webhook/kubernetes"
)

var (
	stageGroupKind = schema.GroupKind{
		Group: kargoapi.GroupVersion.Group,
		Kind:  "Stage",
	}
)

type webhook struct {
	client  client.Client
	decoder admission.Decoder

	// The following behaviors are overridable for testing purposes:

	admissionRequestFromContextFn func(context.Context) (admission.Request, error)

	validateProjectFn func(
		context.Context,
		client.Client,
		client.Object,
	) error

	validateSpecFn func(*field.Path, kargoapi.StageSpec) field.ErrorList

	isRequestFromKargoControlplaneFn libWebhook.IsRequestFromKargoControlplaneFn
}

func SetupWebhookWithManager(
	cfg libWebhook.Config,
	mgr ctrl.Manager,
) error {
	w := newWebhook(
		cfg,
		mgr.GetClient(),
		admission.NewDecoder(mgr.GetScheme()),
	)
	return ctrl.NewWebhookManagedBy(mgr).
		For(&kargoapi.Stage{}).
		WithDefaulter(w).
		WithValidator(w).
		Complete()
}

func newWebhook(
	cfg libWebhook.Config,
	kubeClient client.Client,
	decoder admission.Decoder,
) *webhook {
	w := &webhook{
		client:  kubeClient,
		decoder: decoder,
	}
	w.admissionRequestFromContextFn = admission.RequestFromContext
	w.validateProjectFn = libWebhook.ValidateProject
	w.validateSpecFn = w.validateSpec
	w.isRequestFromKargoControlplaneFn =
		libWebhook.IsRequestFromKargoControlplane(cfg.ControlplaneUserRegex)
	return w
}

func (w *webhook) Default(ctx context.Context, obj runtime.Object) error {
	stage := obj.(*kargoapi.Stage) // nolint: forcetypeassert

	// Sync the shard label to the convenience shard field
	if stage.Spec.Shard != "" {
		if stage.Labels == nil {
			stage.Labels = make(map[string]string, 1)
		}
		stage.Labels[kargoapi.LabelKeyShard] = stage.Spec.Shard
	} else {
		delete(stage.Labels, kargoapi.LabelKeyShard)
	}

	req, err := w.admissionRequestFromContextFn(ctx)
	if err != nil {
		return fmt.Errorf("get admission request from context: %w", err)
	}

	var oldStage *kargoapi.Stage
	// We need to decode old object manually since controller-runtime doesn't decode it for us.
	if req.Operation == admissionv1.Update {
		oldStage = &kargoapi.Stage{}
		if err := w.decoder.DecodeRaw(req.OldObject, oldStage); err != nil {
			return fmt.Errorf("decode old object: %w", err)
		}
	}

	if req.Operation == admissionv1.Create || req.Operation == admissionv1.Update {
		if verReq, ok := api.ReverifyAnnotationValue(stage.Annotations); ok {
			var oldVerReq *kargoapi.VerificationRequest
			if oldStage != nil {
				oldVerReq, _ = api.ReverifyAnnotationValue(oldStage.Annotations)
			}
			// If the re-verification request has changed, enrich the annotation
			// with the actor and control plane information.
			if oldStage == nil || oldVerReq == nil || !verReq.Equals(oldVerReq) {
				verReq.ControlPlane = w.isRequestFromKargoControlplaneFn(req)
				if !verReq.ControlPlane {
					// If the re-verification request is not from the control plane, then
					// it's from a specific Kubernetes user. Without this check we would
					// overwrite the actor field set by the control plane.
					verReq.Actor = api.FormatEventKubernetesUserActor(req.UserInfo)
				}
				stage.Annotations[kargoapi.AnnotationKeyReverify] = verReq.String()
			}
		}

		if verReq, ok := api.AbortVerificationAnnotationValue(stage.Annotations); ok {
			var oldVerReq *kargoapi.VerificationRequest
			if oldStage != nil {
				oldVerReq, _ = api.AbortVerificationAnnotationValue(oldStage.Annotations)
			}
			// If the abort request has changed, enrich the annotation with the
			// actor and control plane information.
			if oldStage == nil || oldVerReq == nil || !verReq.Equals(oldVerReq) {
				verReq.ControlPlane = w.isRequestFromKargoControlplaneFn(req)
				if !verReq.ControlPlane {
					// If the abort request is not from the control plane, then
					// it's from a specific Kubernetes user. Without this check we would
					// overwrite the actor field set by the control plane.
					verReq.Actor = api.FormatEventKubernetesUserActor(req.UserInfo)
				}
				stage.Annotations[kargoapi.AnnotationKeyAbort] = verReq.String()
			}
		}
	}
	return nil
}

func (w *webhook) ValidateCreate(
	ctx context.Context,
	obj runtime.Object,
) (admission.Warnings, error) {
	stage := obj.(*kargoapi.Stage) // nolint: forcetypeassert
	var errs field.ErrorList
	if err := w.validateProjectFn(ctx, w.client, stage); err != nil {
		var statusErr *apierrors.StatusError
		if ok := errors.As(err, &statusErr); ok {
			return nil, statusErr
		}
		var fieldErr *field.Error
		if ok := errors.As(err, &fieldErr); !ok {
			return nil, apierrors.NewInternalError(err)
		}
		errs = append(errs, fieldErr)
	}
	if errs = append(
		errs,
		w.validateSpecFn(field.NewPath("spec"), stage.Spec)...,
	); len(errs) > 0 {
		return nil, apierrors.NewInvalid(stageGroupKind, stage.Name, errs)
	}
	return nil, nil
}

func (w *webhook) ValidateUpdate(
	_ context.Context,
	_ runtime.Object,
	newObj runtime.Object,
) (admission.Warnings, error) {
	stage := newObj.(*kargoapi.Stage) // nolint: forcetypeassert
	if errs := w.validateSpecFn(field.NewPath("spec"), stage.Spec); len(errs) > 0 {
		return nil, apierrors.NewInvalid(stageGroupKind, stage.Name, errs)
	}
	return nil, nil
}

func (w *webhook) ValidateDelete(
	context.Context,
	runtime.Object,
) (admission.Warnings, error) {
	// No-op
	return nil, nil
}

func (w *webhook) validateSpec(
	f *field.Path,
	spec kargoapi.StageSpec,
) field.ErrorList {
	errs := w.validateRequestedFreight(f.Child("requestedFreight"), spec.RequestedFreight)
	if spec.PromotionTemplate == nil {
		return errs
	}
	return append(
		errs,
		libWebhook.ValidatePromotionSteps(
			f.Child("promotionTemplate").Child("spec").Child("steps"),
			spec.PromotionTemplate.Spec.Steps,
		)...,
	)
}

func (w *webhook) validateRequestedFreight(
	f *field.Path,
	reqs []kargoapi.FreightRequest,
) field.ErrorList {
	// Make sure the same origin is not requested multiple times
	seenOrigins := make(map[string]struct{}, len(reqs))
	for _, req := range reqs {
		if _, seen := seenOrigins[req.Origin.String()]; seen {
			return field.ErrorList{
				field.Invalid(
					f,
					reqs,
					fmt.Sprintf(
						"freight with origin %s requested multiple times in %s",
						req.Origin.String(),
						f.String(),
					),
				),
			}
		}
		seenOrigins[req.Origin.String()] = struct{}{}
	}
	return nil
}
