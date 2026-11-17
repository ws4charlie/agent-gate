// AgentGate runtime policy as Cedar. It mirrors policy.Evaluate in
// internal/policy and runs beside it in shadow mode: Cedar decides nothing.
//
// One permit, then one forbid per rule. Each forbid carries @rule, the rule id
// the Go engine reports. When several forbids fire, the one earliest in the
// Go rule order is reported (see internal/cedarpolicy). Limits of 0 mean "no
// limit". The *_total values are summed by the caller so Cedar arithmetic
// cannot overflow.

permit (principal, action in [Action::"call_model", Action::"call_tool", Action::"pay"], resource);

@rule("agent_killed")
forbid (principal, action, resource)
when { principal.status == "killed" };

@rule("agent_revoked")
forbid (principal, action, resource)
when { principal.status != "active" && principal.status != "killed" };

@rule("invalid_request")
forbid (principal, action, resource)
unless { context.valid };

@rule("scope_model")
forbid (principal, action == Action::"call_model", resource)
unless { context.model_in_scope };

@rule("scope_tool")
forbid (principal, action in [Action::"call_tool", Action::"pay"], resource)
unless { context.tool_in_scope };

@rule("scope_payee")
forbid (principal, action == Action::"pay", resource)
unless { context.payee_in_scope };

@rule("budget_daily")
forbid (principal, action, resource)
when { principal.daily_budget > 0 && context.daily_total > principal.daily_budget };

@rule("budget_task")
forbid (principal, action, resource)
when { principal.task_budget > 0 && context.task_total > principal.task_budget };

@rule("velocity")
forbid (principal, action, resource)
when { principal.velocity_per_minute > 0 && context.velocity_total > principal.velocity_per_minute };

@rule("approval_threshold")
@verdict("require_approval")
forbid (principal, action, resource)
when { principal.approval_threshold > 0 && context.amount > principal.approval_threshold }
unless { context.approved };
