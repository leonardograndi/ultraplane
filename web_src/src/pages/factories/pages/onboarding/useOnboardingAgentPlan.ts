import { useHostedLLMModels } from "@/hooks/useHostedLLMModels";
import { hostedModelIds } from "@/lib/hostedLLMModels";
import type { FactoryAgentRewrite } from "@/pages/home/factories";
import type { IntegrationSelections } from "@/pages/home/InstallIntegrationsSection";

import {
  hostedModelsQueriesLoading,
  resolveOnboardingAgent,
  type OnboardingAgentPlan,
} from "./onboardingAgentReadiness";
import type { IntegrationId } from "./onboardingFixtures";

export function useOnboardingAgentPlan(
  organizationId: string,
  connected: Set<IntegrationId>,
  remainingCreditCents: number,
  runnerLogin = false,
) {
  const needHosted = remainingCreditCents > 0 && !runnerLogin;
  const anthropic = useHostedLLMModels(organizationId, "anthropic", needHosted);
  const openai = useHostedLLMModels(organizationId, "openai", needHosted);
  const openrouter = useHostedLLMModels(organizationId, "openrouter", needHosted);
  return {
    remainingCreditCents,
    hostedModelsLoading: hostedModelsQueriesLoading(needHosted, [anthropic, openai, openrouter]),
    plan: resolveOnboardingAgent({
      connected,
      remainingCreditCents,
      runnerLogin,
      hostedModels: {
        anthropic: hostedModelIds(anthropic.data?.models),
        openai: hostedModelIds(openai.data?.models),
        openrouter: hostedModelIds(openrouter.data?.models),
      },
    }),
  };
}

export function agentRewriteFromPlan(
  plan: OnboardingAgentPlan,
  selections: IntegrationSelections,
): FactoryAgentRewrite {
  if (plan.credentialsSource === "hosted") {
    return {
      component: plan.component,
      model: plan.model,
      planningModel: plan.planningModel,
      credentials: { source: "hosted" },
    };
  }
  if (plan.credentialsSource === "runner") {
    return {
      component: plan.component,
      model: plan.model,
      planningModel: plan.planningModel,
      credentials: { source: "runner" },
    };
  }
  return {
    component: plan.component,
    model: plan.model,
    planningModel: plan.planningModel,
    credentials: {
      source: "integration",
      name: selections[plan.integrationName]?.name ?? plan.integrationName,
    },
  };
}
