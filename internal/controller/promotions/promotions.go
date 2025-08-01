package promotions

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/kelseyhightower/envconfig"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/source"

	kargoapi "github.com/akuity/kargo/api/v1alpha1"
	"github.com/akuity/kargo/internal/api"
	"github.com/akuity/kargo/internal/controller"
	argocd "github.com/akuity/kargo/internal/controller/argocd/api/v1alpha1"
	"github.com/akuity/kargo/internal/event"
	"github.com/akuity/kargo/internal/indexer"
	"github.com/akuity/kargo/internal/kargo"
	"github.com/akuity/kargo/internal/kubeclient"
	libEvent "github.com/akuity/kargo/internal/kubernetes/event"
	"github.com/akuity/kargo/internal/logging"
	intpredicate "github.com/akuity/kargo/internal/predicate"
	"github.com/akuity/kargo/internal/promotion"
	pkgPromotion "github.com/akuity/kargo/pkg/promotion"
)

// ReconcilerConfig represents configuration for the promotion reconciler.
type ReconcilerConfig struct {
	IsDefaultController     bool   `envconfig:"IS_DEFAULT_CONTROLLER"`
	ShardName               string `envconfig:"SHARD_NAME"`
	APIServerBaseURL        string `envconfig:"API_SERVER_BASE_URL"`
	MaxConcurrentReconciles int    `envconfig:"MAX_CONCURRENT_PROMOTION_RECONCILES" default:"4"`
}

func (c ReconcilerConfig) Name() string {
	name := "promotion-controller"
	if c.ShardName != "" {
		return name + "-" + c.ShardName
	}
	return name
}

func ReconcilerConfigFromEnv() ReconcilerConfig {
	var cfg ReconcilerConfig
	envconfig.MustProcess("", &cfg)
	return cfg
}

// reconciler reconciles Promotion resources.
type reconciler struct {
	kargoClient client.Client
	promoEngine promotion.Engine

	cfg ReconcilerConfig

	recorder record.EventRecorder

	// The following behaviors are overridable for testing purposes:

	getStageFn func(
		context.Context,
		client.Client,
		types.NamespacedName,
	) (*kargoapi.Stage, error)

	promoteFn func(
		context.Context,
		kargoapi.Promotion,
		*kargoapi.Stage,
		*kargoapi.Freight,
	) (*kargoapi.PromotionStatus, error)

	terminatePromotionFn func(
		context.Context,
		*kargoapi.AbortPromotionRequest,
		*kargoapi.Promotion,
		*kargoapi.Freight,
	) error
}

// SetupReconcilerWithManager initializes a reconciler for Promotion resources
// and registers it with the provided Manager.
func SetupReconcilerWithManager(
	ctx context.Context,
	kargoMgr manager.Manager,
	argocdMgr manager.Manager,
	promoEngine promotion.Engine,
	cfg ReconcilerConfig,
) error {
	// Index running Promotions by Argo CD Applications
	if err := kargoMgr.GetFieldIndexer().IndexField(
		ctx,
		&kargoapi.Promotion{},
		indexer.RunningPromotionsByArgoCDApplicationsField,
		indexer.RunningPromotionsByArgoCDApplications(
			ctx,
			kargoMgr.GetClient(),
			cfg.ShardName,
			cfg.IsDefaultController,
		),
	); err != nil {
		return fmt.Errorf("index running Promotions by Argo CD Applications: %w", err)
	}

	reconciler := newReconciler(
		kargoMgr.GetClient(),
		libEvent.NewRecorder(ctx, kargoMgr.GetScheme(), kargoMgr.GetClient(), cfg.Name()),
		promoEngine,
		cfg,
	)

	c, err := ctrl.NewControllerManagedBy(kargoMgr).
		For(&kargoapi.Promotion{}).
		WithEventFilter(controller.ResponsibleFor[client.Object]{
			IsDefaultController: cfg.IsDefaultController,
			ShardName:           cfg.ShardName,
		}).
		WithEventFilter(intpredicate.IgnoreDelete[client.Object]{}).
		WithEventFilter(predicate.Or(
			predicate.GenerationChangedPredicate{},
			kargo.RefreshRequested{},
			kargo.PromotionAbortRequested{},
		)).
		WithOptions(controller.CommonOptions(cfg.MaxConcurrentReconciles)).
		Build(reconciler)
	if err != nil {
		return fmt.Errorf("error building Promotion controller: %w", err)
	}

	logger := logging.LoggerFromContext(ctx)

	// Watch Stages that acknowledge their next Promotion and enqueue it.
	if err = c.Watch(
		source.Kind(
			kargoMgr.GetCache(),
			&kargoapi.Stage{},
			&PromotionAcknowledgedByStageHandler[*kargoapi.Stage]{},
		),
	); err != nil {
		return fmt.Errorf("unable to watch Stages: %w", err)
	}

	// If Argo CD integration is disabled, this manager will be nil, and we won't
	// care about this watch anyway.
	if argocdMgr != nil {
		if err = c.Watch(
			source.Kind(
				argocdMgr.GetCache(),
				&argocd.Application{},
				&UpdatedArgoCDAppHandler[*argocd.Application]{
					kargoClient: kargoMgr.GetClient(),
				},
				ArgoCDAppOperationCompleted[*argocd.Application]{
					logger: logger,
				},
			),
		); err != nil {
			return fmt.Errorf("unable to watch Applications: %w", err)
		}
	}

	logging.LoggerFromContext(ctx).Info(
		"Initialized Promotion reconciler",
		"maxConcurrentReconciles", cfg.MaxConcurrentReconciles,
	)

	return nil
}

func newReconciler(
	kargoClient client.Client,
	recorder record.EventRecorder,
	promoEngine promotion.Engine,
	cfg ReconcilerConfig,
) *reconciler {
	r := &reconciler{
		kargoClient: kargoClient,
		promoEngine: promoEngine,
		recorder:    recorder,
		cfg:         cfg,
	}
	r.getStageFn = api.GetStage
	r.promoteFn = r.promote
	r.terminatePromotionFn = r.terminatePromotion
	return r
}

// Reconcile is part of the main Kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *reconciler) Reconcile(
	ctx context.Context,
	req ctrl.Request,
) (ctrl.Result, error) {
	logger := logging.LoggerFromContext(ctx).WithValues(
		"namespace", req.Namespace,
		"promotion", req.Name,
	)
	ctx = logging.ContextWithLogger(ctx, logger)
	logger.Debug("reconciling Promotion")

	// Find the Promotion
	promo, err := api.GetPromotion(ctx, r.kargoClient, req.NamespacedName)
	if err != nil {
		return ctrl.Result{}, err
	}
	if promo == nil || promo.Status.Phase.IsTerminal() {
		// Ignore if not found or already finished. Promo might be nil if the
		// Promotion was deleted after the current reconciliation request was issued.
		return ctrl.Result{}, nil
	}
	// Find the Freight
	freight, err := api.GetFreight(ctx, r.kargoClient, types.NamespacedName{
		Namespace: promo.Namespace,
		Name:      promo.Spec.Freight,
	})
	if err != nil {
		return ctrl.Result{}, fmt.Errorf(
			"error finding Freight %q in namespace %q: %w",
			promo.Spec.Freight,
			promo.Namespace,
			err,
		)
	}

	logger = logger.WithValues(
		"namespace", req.Namespace,
		"promotion", req.Name,
		"stage", promo.Spec.Stage,
		"freight", promo.Spec.Freight,
	)

	// Terminate the Promotion if requested by the user.
	if req, ok := api.AbortPromotionAnnotationValue(
		promo.GetAnnotations(),
	); ok && req.Action == kargoapi.AbortActionTerminate {
		if err = r.terminatePromotionFn(ctx, req, promo, freight); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	// If the Promotion does not have a Phase, it must be new and (initially)
	// pending. Mark it as such.
	if promo.Status.Phase == "" {
		if err = kubeclient.PatchStatus(ctx, r.kargoClient, promo, func(status *kargoapi.PromotionStatus) {
			status.Phase = kargoapi.PromotionPhasePending
		}); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Retrieve the Stage associated with the Promotion.
	stage, err := r.getStageFn(
		ctx,
		r.kargoClient,
		types.NamespacedName{
			Namespace: promo.Namespace,
			Name:      promo.Spec.Stage,
		},
	)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf(
			"error finding Stage %q in namespace %q: %w",
			promo.Spec.Stage, promo.Namespace, err,
		)
	}
	if stage == nil {
		return ctrl.Result{}, fmt.Errorf(
			"could not find Stage %q in namespace %q",
			promo.Spec.Stage, promo.Namespace,
		)
	}

	// Confirm that the Stage is awaiting this Promotion.
	// This effectively prevents the Promotion from running until the Stage
	// decides it is the next Promotion to run.
	if stage.Status.CurrentPromotion == nil || stage.Status.CurrentPromotion.Name != promo.Name {
		// The watch on the Stage will requeue the Promotion if the Stage
		// acknowledges it.
		logger.Debug("Stage is not awaiting Promotion", "stage", stage.Name, "promotion", promo.Name)
		return ctrl.Result{}, nil
	}

	// Update promo status as Running to give visibility in UI. Also, a promo which
	// has already entered Running status will be allowed to continue to reconcile.
	if promo.Status.Phase != kargoapi.PromotionPhaseRunning {
		if err = kubeclient.PatchStatus(ctx, r.kargoClient, promo, func(status *kargoapi.PromotionStatus) {
			status.Phase = kargoapi.PromotionPhaseRunning
			status.StartedAt = &metav1.Time{Time: time.Now()}
		}); err != nil {
			return ctrl.Result{}, err
		}
		logger.Info("began promotion")
	} else {
		logger.Debug("continuing Promotion")
	}

	promoCtx := logging.ContextWithLogger(ctx, logger)

	newStatus := promo.Status.DeepCopy()

	// Wrap the promoteFn() call in an anonymous function to recover() any panics, so
	// we can update the promo's phase with Error if it does. This breaks an infinite
	// cycle of a bad promo continuously failing to reconcile, and surfaces the error.
	func() {
		defer func() {
			if err := recover(); err != nil {
				if theErr, ok := err.(error); ok {
					logger.Error(theErr, "Promotion panic")
				} else {
					logger.Error(nil, "Promotion panic")
				}
				newStatus.Phase = kargoapi.PromotionPhaseErrored
				newStatus.Message = fmt.Sprintf("%v", err)
			}
		}()
		otherStatus, promoteErr := r.promoteFn(
			promoCtx,
			*promo,
			stage,
			freight,
		)
		if otherStatus != nil {
			newStatus = otherStatus
		}
		if promoteErr != nil {
			newStatus.Phase = kargoapi.PromotionPhaseErrored
			newStatus.Message = promoteErr.Error()
			logger.Error(promoteErr, "error executing Promotion")
		}
	}()

	if newStatus.Phase.IsTerminal() {
		newStatus.FinishedAt = &metav1.Time{Time: time.Now()}
		logger.Info("promotion", "phase", newStatus.Phase)
	}

	// Record the current refresh token as having been handled.
	if token, ok := api.RefreshAnnotationValue(promo.GetAnnotations()); ok {
		newStatus.LastHandledRefresh = token
	}

	if err = kubeclient.PatchStatus(ctx, r.kargoClient, promo, func(status *kargoapi.PromotionStatus) {
		*status = *newStatus
	}); err != nil {
		logger.Error(err, "error updating Promotion status")

		if apierrors.IsInvalid(err) {
			// If the error is due to an invalid status update, we should mark
			// the Promotion as errored to prevent it from being requeued.
			//
			// NB: This should be a rare occurrence, and is either due to the
			// CustomResourceDefinition being out of sync with the controller
			// version, or us inventing non-backwards-compatible changes.
			err = kubeclient.PatchStatus(ctx, r.kargoClient, promo, func(status *kargoapi.PromotionStatus) {
				status.Phase = kargoapi.PromotionPhaseErrored
				status.Message = fmt.Sprintf("error updating status: %v", err)
			})
		}
	}

	// Record event after patching status if new phase is terminal
	if newStatus.Phase.IsTerminal() {
		stage, getStageErr := r.getStageFn(
			ctx,
			r.kargoClient,
			types.NamespacedName{
				Namespace: promo.Namespace,
				Name:      promo.Spec.Stage,
			},
		)
		if getStageErr != nil {
			return ctrl.Result{}, fmt.Errorf("get stage: %w", err)
		}
		if stage == nil {
			return ctrl.Result{}, fmt.Errorf(
				"stage %q not found in namespace %q",
				promo.Spec.Stage,
				promo.Namespace,
			)
		}

		var reason string
		switch newStatus.Phase {
		case kargoapi.PromotionPhaseSucceeded:
			reason = kargoapi.EventReasonPromotionSucceeded
		case kargoapi.PromotionPhaseFailed:
			reason = kargoapi.EventReasonPromotionFailed
		case kargoapi.PromotionPhaseErrored:
			reason = kargoapi.EventReasonPromotionErrored
		}

		msg := fmt.Sprintf("Promotion %s", newStatus.Phase)
		if newStatus.Message != "" {
			msg += fmt.Sprintf(": %s", newStatus.Message)
		}

		eventAnnotations := event.NewPromotionAnnotations(ctx,
			api.FormatEventControllerActor(r.cfg.Name()),
			promo, freight)

		if newStatus.Phase == kargoapi.PromotionPhaseSucceeded {
			eventAnnotations[kargoapi.AnnotationKeyEventVerificationPending] =
				strconv.FormatBool(stage.Spec.Verification != nil)
		}
		r.recorder.AnnotatedEventf(promo, eventAnnotations, corev1.EventTypeNormal, reason, msg)
	}

	if err != nil {
		// Controller runtime automatically gives us a progressive backoff if err is
		// not nil
		return ctrl.Result{}, err
	}

	// If the promotion is still running, we'll need to periodically check on
	// it.
	if newStatus.Phase == kargoapi.PromotionPhaseRunning {
		return ctrl.Result{RequeueAfter: calculateRequeueInterval(promo)}, nil
	}
	return ctrl.Result{}, nil
}

func (r *reconciler) promote(
	ctx context.Context,
	promo kargoapi.Promotion,
	stage *kargoapi.Stage,
	targetFreight *kargoapi.Freight,
) (*kargoapi.PromotionStatus, error) {
	logger := logging.LoggerFromContext(ctx)
	stageName := stage.Name
	stageNamespace := promo.Namespace

	if targetFreight == nil {
		// nolint:staticcheck
		return nil, fmt.Errorf("Freight %q not found in namespace %q", promo.Spec.Freight, promo.Namespace)
	}

	if !stage.IsFreightAvailable(targetFreight) {
		// nolint:staticcheck
		return nil, fmt.Errorf(
			"Freight %q is not available to Stage %q in namespace %q",
			promo.Spec.Freight,
			stageName,
			stageNamespace,
		)
	}

	logger = logger.WithValues("targetFreight", targetFreight.Name)

	targetFreightRef := kargoapi.FreightReference{
		Name:    targetFreight.Name,
		Commits: targetFreight.Commits,
		Images:  targetFreight.Images,
		Charts:  targetFreight.Charts,
		Origin:  targetFreight.Origin,
	}

	// Make a deep copy of the Promotion to pass to the promotion steps execution
	// engine, which may modify its status.
	workingPromo := promo.DeepCopy()
	workingPromo.Status.Freight = &targetFreightRef
	workingPromo.Status.FreightCollection = r.buildTargetFreightCollection(
		ctx,
		targetFreightRef,
		stage,
	)

	// Prepare promotion steps and vars for the promotion execution engine.
	steps := make([]promotion.Step, len(workingPromo.Spec.Steps))
	for i, step := range workingPromo.Spec.Steps {
		steps[i] = promotion.Step{
			Kind:            step.Uses,
			Alias:           step.As,
			If:              step.If,
			ContinueOnError: step.ContinueOnError,
			Retry:           step.Retry,
			Vars:            step.Vars,
			Config:          step.Config.Raw,
		}
	}

	promoCtx := promotion.Context{
		UIBaseURL:             r.cfg.APIServerBaseURL,
		WorkDir:               filepath.Join(os.TempDir(), "promotion-"+string(workingPromo.UID)),
		Project:               stageNamespace,
		Stage:                 stageName,
		Promotion:             workingPromo.Name,
		FreightRequests:       stage.Spec.RequestedFreight,
		Freight:               *workingPromo.Status.FreightCollection.DeepCopy(),
		TargetFreightRef:      targetFreightRef,
		StartFromStep:         promo.Status.CurrentStep,
		StepExecutionMetadata: promo.Status.StepExecutionMetadata,
		State:                 pkgPromotion.State(workingPromo.Status.GetState()),
		Vars:                  workingPromo.Spec.Vars,
		Actor:                 parseCreateActorAnnotation(&promo),
	}
	if err := os.Mkdir(promoCtx.WorkDir, 0o700); err == nil {
		// If we're working with a fresh directory, we should start the promotion
		// process again from the beginning, but we DON'T clear shared state. This
		// allows individual steps to self-discover that they've run before and
		// examine the results of their own previous execution.
		promoCtx.StartFromStep = 0
		promoCtx.StepExecutionMetadata = nil
		workingPromo.Status.HealthChecks = nil
	} else if !os.IsExist(err) {
		return nil, fmt.Errorf("error creating working directory: %w", err)
	}
	defer func() {
		if workingPromo.Status.Phase.IsTerminal() {
			if err := os.RemoveAll(promoCtx.WorkDir); err != nil {
				logger.Error(err, "could not remove working directory")
			}
		}
	}()

	res, err := r.promoEngine.Promote(ctx, promoCtx, steps)
	workingPromo.Status.Phase = res.Status
	workingPromo.Status.Message = res.Message
	workingPromo.Status.CurrentStep = res.CurrentStep
	workingPromo.Status.StepExecutionMetadata = res.StepExecutionMetadata
	workingPromo.Status.State = &apiextensionsv1.JSON{Raw: res.State.ToJSON()}
	for _, step := range res.HealthChecks {
		workingPromo.Status.HealthChecks = append(
			workingPromo.Status.HealthChecks,
			kargoapi.HealthCheckStep{
				Uses:   step.Kind,
				Config: &apiextensionsv1.JSON{Raw: step.Input.ToJSON()},
			},
		)
	}
	if err != nil {
		workingPromo.Status.Phase = kargoapi.PromotionPhaseErrored
		return &workingPromo.Status, err
	}

	logger.Debug("promotion", "phase", workingPromo.Status.Phase)

	if workingPromo.Status.Phase == kargoapi.PromotionPhaseSucceeded {
		// Trigger re-verification of the Stage if the promotion succeeded and
		// this is a re-promotion of the same Freight.
		current := stage.Status.FreightHistory.Current()
		if current != nil && current.VerificationHistory.Current() != nil {
			for _, f := range current.Freight {
				if f.Name == targetFreight.Name {
					if err := api.ReverifyStageFreight(
						ctx,
						r.kargoClient,
						types.NamespacedName{
							Namespace: stageNamespace,
							Name:      stageName,
						},
					); err != nil {
						// Log the error, but don't let failure to initiate re-verification
						// prevent the promotion from succeeding.
						logger.Error(err, "error triggering re-verification")
					}
					break
				}
			}
		}
	}

	return &workingPromo.Status, nil
}

// buildTargetFreightCollection constructs a FreightCollection that contains all
// FreightReferences from the previous Promotion (excepting those that are no
// longer requested), plus a FreightReference for the provided targetFreight.
func (r *reconciler) buildTargetFreightCollection(
	ctx context.Context,
	targetFreight kargoapi.FreightReference,
	stage *kargoapi.Stage,
) *kargoapi.FreightCollection {
	logger := logging.LoggerFromContext(ctx)
	freightCol := &kargoapi.FreightCollection{}

	// We don't simply copy the current FreightCollection because we want to
	// account for the possibility that some freight contained therein are no
	// longer requested by the Stage.
	if len(stage.Spec.RequestedFreight) > 1 {
		lastPromo := stage.Status.LastPromotion
		if lastPromo.Status != nil && lastPromo.Status.FreightCollection != nil {
			for _, req := range stage.Spec.RequestedFreight {
				if freight, ok := lastPromo.Status.FreightCollection.Freight[req.Origin.String()]; ok {
					freightCol.UpdateOrPush(freight)
				}
			}
		} else {
			logger.Debug("last promotion has no collection to inherit Freight from")
		}
	}
	freightCol.UpdateOrPush(targetFreight)
	return freightCol
}

// terminatePromotion terminates the given Promotion with a message indicating
// that it was terminated on user request. It does nothing if the Promotion is
// already in a terminal phase.
func (r *reconciler) terminatePromotion(
	ctx context.Context,
	req *kargoapi.AbortPromotionRequest,
	promo *kargoapi.Promotion,
	freight *kargoapi.Freight,
) error {
	logger := logging.LoggerFromContext(ctx)

	if promo.Status.Phase.IsTerminal() {
		logger.Debug("can not terminate Promotion in terminal phase", "phase", promo.Status.Phase)
		return nil
	}

	logger.Info("terminating Promotion")

	// Normally, the actor is inherited from the creator of the Promotion for
	// events. For an abort request, however, we do not want to inherit this
	// as the abort request is not necessarily made by the creator of the
	// Promotion.
	actor := api.FormatEventControllerActor(r.cfg.Name())
	if req.Actor != "" {
		actor = req.Actor
	}

	newStatus := promo.Status.DeepCopy()

	now := &metav1.Time{Time: time.Now()}

	// If a step was running, mark the step as aborted.
	if newStatus.Phase == kargoapi.PromotionPhaseRunning &&
		int64(len(newStatus.StepExecutionMetadata)) == promo.Status.CurrentStep+1 &&
		promo.Status.StepExecutionMetadata[promo.Status.CurrentStep].Status == kargoapi.PromotionStepStatusRunning {
		newStatus.StepExecutionMetadata[promo.Status.CurrentStep].Status = kargoapi.PromotionStepStatusAborted
		newStatus.StepExecutionMetadata[promo.Status.CurrentStep].FinishedAt = now
	}

	newStatus.Phase = kargoapi.PromotionPhaseAborted
	if actor != "" {
		newStatus.Message = fmt.Sprintf("Promotion terminated by %s", actor)
	} else {
		newStatus.Message = "Promotion terminated per user request"
	}
	newStatus.FinishedAt = now

	if err := kubeclient.PatchStatus(ctx, r.kargoClient, promo, func(status *kargoapi.PromotionStatus) {
		*status = *newStatus
	}); err != nil {
		return err
	}

	eventMeta := event.NewPromotionAnnotations(ctx, "", promo, freight)
	eventMeta[kargoapi.AnnotationKeyEventActor] = actor

	r.recorder.AnnotatedEventf(
		promo,
		eventMeta,
		corev1.EventTypeNormal,
		kargoapi.EventReasonPromotionAborted,
		newStatus.Message,
	)

	return nil
}

// parseCreateActorAnnotation extracts the v1alpha1.AnnotationKeyCreateActor
// value from the Promotion's annotations and returns it. If the value contains
// a colon, it is split and the second part is returned. Otherwise, the entire
// value or an empty string is returned.
func parseCreateActorAnnotation(promo *kargoapi.Promotion) string {
	var creator string
	if v, ok := promo.Annotations[kargoapi.AnnotationKeyCreateActor]; ok {
		if v != kargoapi.EventActorUnknown {
			creator = v
		}
		if parts := strings.Split(v, ":"); len(parts) == 2 {
			creator = parts[1]
		}
	}
	return creator
}

var defaultRequeueInterval = 5 * time.Minute

func calculateRequeueInterval(p *kargoapi.Promotion) time.Duration {
	// Ensure we have a step for the current step index.
	if int(p.Status.CurrentStep) >= len(p.Spec.Steps) {
		return defaultRequeueInterval
	}

	step := p.Spec.Steps[p.Status.CurrentStep]
	runner := promotion.GetStepRunner(step.Uses)

	timeout := (&promotion.Step{
		Retry: step.Retry,
	}).GetTimeout(runner)

	// If there is no timeout, or the timeout is 0, we should requeue at the
	// default interval.
	if timeout == nil || *timeout == 0 {
		return defaultRequeueInterval
	}

	// Ensure we have an execution metadata entry for the current step.
	if int(p.Status.CurrentStep) >= len(p.Status.StepExecutionMetadata) {
		return defaultRequeueInterval
	}

	md := p.Status.StepExecutionMetadata[p.Status.CurrentStep]
	targetTimeout := md.StartedAt.Add(*timeout)
	if targetTimeout.Before(time.Now().Add(defaultRequeueInterval)) {
		return time.Until(targetTimeout)
	}
	return defaultRequeueInterval
}
