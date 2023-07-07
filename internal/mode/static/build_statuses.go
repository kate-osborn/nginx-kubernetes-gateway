package static

import (
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/gateway-api/apis/v1beta1"

	"github.com/nginxinc/nginx-kubernetes-gateway/internal/framework/conditions"
	"github.com/nginxinc/nginx-kubernetes-gateway/internal/framework/status"
	"github.com/nginxinc/nginx-kubernetes-gateway/internal/mode/static/state/graph"
)

type nginxReloadResult struct {
	error error
}

// buildStatuses builds status.Statuses from a Graph.
func buildStatuses(graph *graph.Graph, nginxReloadRes nginxReloadResult) status.Statuses {
	statuses := status.Statuses{
		HTTPRouteStatuses: make(status.HTTPRouteStatuses),
	}

	statuses.GatewayClassStatuses = buildGatewayClassStatuses(graph.GatewayClass, graph.IgnoredGatewayClasses)

	statuses.GatewayStatuses = buildGatewayStatuses(graph.Gateway, graph.IgnoredGateways, nginxReloadRes)

	for nsname, r := range graph.Routes {
		parentStatuses := make([]status.ParentStatus, 0, len(r.ParentRefs))

		defaultConds := conditions.NewDefaultRouteConditions()

		for _, ref := range r.ParentRefs {
			failedAttachmentCondCount := 0
			if ref.Attachment != nil && !ref.Attachment.Attached {
				failedAttachmentCondCount = 1
			}
			allConds := make([]conditions.Condition, 0, len(r.Conditions)+len(defaultConds)+failedAttachmentCondCount)

			// We add defaultConds first, so that any additional conditions will override them, which is
			// ensured by DeduplicateConditions.
			allConds = append(allConds, defaultConds...)
			allConds = append(allConds, r.Conditions...)
			if failedAttachmentCondCount == 1 {
				allConds = append(allConds, ref.Attachment.FailedCondition)
			}

			if nginxReloadRes.error != nil {
				allConds = append(
					allConds,
					conditions.NewRouteGatewayNotProgrammed(conditions.RouteMessageFailedNginxReload),
				)
			}

			routeRef := r.Source.Spec.ParentRefs[ref.Idx]

			parentStatuses = append(parentStatuses, status.ParentStatus{
				GatewayNsName: ref.Gateway,
				SectionName:   routeRef.SectionName,
				Conditions:    conditions.DeduplicateConditions(allConds),
			})
		}

		statuses.HTTPRouteStatuses[nsname] = status.HTTPRouteStatus{
			ObservedGeneration: r.Source.Generation,
			ParentStatuses:     parentStatuses,
		}
	}

	return statuses
}

func buildGatewayClassStatuses(
	gc *graph.GatewayClass,
	ignoredGwClasses map[types.NamespacedName]*v1beta1.GatewayClass,
) status.GatewayClassStatuses {
	statuses := make(status.GatewayClassStatuses)

	if gc != nil {
		defaultConds := conditions.NewDefaultGatewayClassConditions()

		conds := make([]conditions.Condition, 0, len(gc.Conditions)+len(defaultConds))

		// We add default conds first, so that any additional conditions will override them, which is
		// ensured by DeduplicateConditions.
		conds = append(conds, defaultConds...)
		conds = append(conds, gc.Conditions...)

		statuses[client.ObjectKeyFromObject(gc.Source)] = status.GatewayClassStatus{
			Conditions:         conditions.DeduplicateConditions(conds),
			ObservedGeneration: gc.Source.Generation,
		}
	}

	for nsname, gwClass := range ignoredGwClasses {
		statuses[nsname] = status.GatewayClassStatus{
			Conditions:         []conditions.Condition{conditions.NewGatewayClassConflict()},
			ObservedGeneration: gwClass.Generation,
		}
	}

	return statuses
}

func buildGatewayStatuses(
	gateway *graph.Gateway,
	ignoredGateways map[types.NamespacedName]*v1beta1.Gateway,
	nginxReloadRes nginxReloadResult,
) status.GatewayStatuses {
	statuses := make(status.GatewayStatuses)

	if gateway != nil {
		statuses[client.ObjectKeyFromObject(gateway.Source)] = buildGatewayStatus(gateway, nginxReloadRes)
	}

	for nsname, gw := range ignoredGateways {
		statuses[nsname] = status.GatewayStatus{
			Conditions:         conditions.NewGatewayConflict(),
			ObservedGeneration: gw.Generation,
		}
	}

	return statuses
}

func buildGatewayStatus(gateway *graph.Gateway, nginxReloadRes nginxReloadResult) status.GatewayStatus {
	if !gateway.Valid {
		return status.GatewayStatus{
			Conditions:         conditions.DeduplicateConditions(gateway.Conditions),
			ObservedGeneration: gateway.Source.Generation,
		}
	}

	listenerStatuses := make(map[string]status.ListenerStatus)

	validListenerCount := 0
	for name, l := range gateway.Listeners {
		var conds []conditions.Condition

		if l.Valid {
			conds = conditions.NewDefaultListenerConditions()
			validListenerCount++
		} else {
			conds = l.Conditions
		}

		if nginxReloadRes.error != nil {
			conds = append(
				conds,
				conditions.NewListenerNotProgrammedInvalid(conditions.ListenerMessageFailedNginxReload),
			)
		}

		listenerStatuses[name] = status.ListenerStatus{
			AttachedRoutes: int32(len(l.Routes)),
			Conditions:     conditions.DeduplicateConditions(conds),
			SupportedKinds: l.SupportedKinds,
		}
	}

	gwConds := conditions.NewDefaultGatewayConditions()
	if validListenerCount == 0 {
		gwConds = append(gwConds, conditions.NewGatewayNotAcceptedListenersNotValid()...)
	} else if validListenerCount < len(gateway.Listeners) {
		gwConds = append(gwConds, conditions.NewGatewayAcceptedListenersNotValid())
	}

	if nginxReloadRes.error != nil {
		gwConds = append(gwConds, conditions.NewGatewayNotProgrammedInvalid(conditions.GatewayMessageFailedNginxReload))
	}

	return status.GatewayStatus{
		Conditions:         conditions.DeduplicateConditions(gwConds),
		ListenerStatuses:   listenerStatuses,
		ObservedGeneration: gateway.Source.Generation,
	}
}
