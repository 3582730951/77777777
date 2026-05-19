# LLM Pool Admin UI Design System

## 1. Design thesis

LLM Pool Admin is an operator console, not a reporting dashboard. Every page must answer one question first:

> What should I do next?

The UI therefore prioritizes decision clarity over raw information density. Dense data is still available, but it lives behind progressive disclosure, tables, and drill-downs.

## 2. References and borrowed rules

This pass additionally anchors the console language to the products the team asked us to benchmark:

- Cloudflare Dashboard: streamlined homepage thinking, fast entry points, and consistent analytics/settings surfaces.
  - https://developers.cloudflare.com/changelog/2026-01-20-kv-dash-ui-homepage/
  - https://github.com/cloudflare/cf-ui
- Tencent TDesign: enterprise Design Token discipline, component consistency, and dark-mode-ready UI foundations.
  - https://github.com/Tencent/tdesign
- Alibaba Cloud B-Design: cloud console standardization across many products, theme color algorithms, and reusable enterprise components.
  - https://ifdesign.com/en/winner-ranking/project/alibaba-cloud-b-design/570355
- Apple Human Interface Guidelines: clarity, deference, legibility, alignment, and single-focus visual hierarchy.
  - https://developer.apple.com/design/human-interface-guidelines/
  - https://developer.apple.com/design/tips/

Derived control-plane rules:

1. **Cloudflare rule: state first, navigation second.** Every page should start with current operational state and the next action, not a marketing headline.
2. **Tencent/Alibaba rule: table and form pages need toolbars.** Filters belong with the table they control; advanced configuration belongs in rails or disclosures.
3. **Apple rule: one dominant focus.** Reduce decorative panels, duplicate CTAs, and explanatory text that competes with the primary task.
4. **Cloud console rule: numbers need shape.** Replace raw numbers with bars, rings, timelines, status dots, and inline empty-state next steps.
5. **SOC/operator rule: abnormalities sort visually above normal data.** Low quota, no quota, bans, failed probes, and errors must be easier to find than healthy records.

- Atlassian Design System: clear product navigation, page-level hierarchy, and explicit affordances.
  - https://atlassian.design/foundations/
- IBM Carbon Design System: enterprise data tables, density discipline, and predictable spacing.
  - https://carbondesignsystem.com/components/data-table/usage/
- Shopify Polaris: admin resource management patterns, filter-first inventory pages, and merchant/operator workflows.
  - https://polaris.shopify.com/components/tables/index-table
  - https://polaris.shopify.com/components/selection-and-input/filters
- Microsoft Fluent 2: command surfaces, restrained depth, and enterprise productivity language.
  - https://fluent2.microsoft.design/
- Material Design: progressive disclosure, navigation hierarchy, and scannable surfaces.
  - https://m3.material.io/
- Ant Design: enterprise admin information architecture, form/table consistency, and operation feedback.
  - https://ant.design/docs/spec/introduce
- Linear / Vercel / GitHub: calm SaaS productivity surfaces, strong command hierarchy, and reduced chrome.

## 3. Core principles

### Decision-first

The top 25% of every page must contain:

1. Current system state.
2. The next recommended action.
3. One primary CTA.
4. At most one secondary CTA.

### One dominant surface

Avoid five equal-weight panels competing for attention. Each page gets one dominant surface:

- Dashboard: `Operator brief`.
- Accounts: `Inventory command`.

Secondary data becomes strips, tables, or disclosure sections.

### Progressive disclosure

Default view shows action-critical information only:

- Health / attention count.
- Throughput / latency / cache.
- Provider capacity.
- Account status / quota / load.

Long-tail analytics, token trends, and detailed health distribution stay collapsed.

### Enterprise density without clutter

Use dense rows for repetitive resources and calm cards for decisions:

- Cards: decisions, summaries, exceptions.
- Tables/lists: inventory and repeatable records.
- Details: low-frequency analysis.

### Status semantics

Use text + color together:

- Green: healthy / ready.
- Amber: low quota / needs planning.
- Red: no quota / banned / blocking.
- Blue: live / neutral action.
- Gray: inactive / no data.

## 4. Layout rules

- Base grid: 8px spacing.
- Page gap: 14-18px.
- Main content max width: 1480px.
- Primary page surface: 2-column on desktop, 1-column under 1100px.
- Metrics: compact horizontal strip, never the hero.
- Tables: one row = one resource, with the primary identifier first and actions last.

## 5. Component vocabulary

### Operator brief

Used on overview pages. It contains:

- Eyebrow.
- Decision title.
- Short state explanation.
- Primary/secondary actions.
- Recommended action list.

### Signal strip

Compact metrics directly below the operator brief. Use it for five or fewer health signals.

### Inventory command

Used on resource pools. It contains:

- Inventory state title.
- Search and view mode controls.
- Health score.
- Issue breakdown.
- Processing order.
- Filter toolbar.

### Resource table

Rows should support scanning and action:

- Identifier + metadata.
- Provider tag.
- Status badge.
- Quota bars.
- Runtime load.
- Last success.
- Row-level actions.

## 6. Anti-patterns to avoid

- Equal-weight KPI card walls.
- Multiple unrelated cards stacked before the primary task.
- Large empty charts without action context.
- Repeating the same controls in topbar and page body.
- Showing low-frequency analytics above the fold.
- Tables with numbers but no recommended action.

## 7. LLM Pool Admin design language

### Product metaphor

LLM Pool Admin is a **mission-control console for account capacity**, not a BI dashboard.
The operator is usually trying to do one of four jobs:

1. Restore capacity.
2. Add or rotate accounts.
3. Define a routing boundary.
4. Issue a credential or verify usage.

Each page should name the job in the hero and make the primary operation visually obvious.

### Tone

- Calm enterprise blue, restrained gradients, low-shadow surfaces.
- High contrast text hierarchy, no decorative cards competing with the main task.
- Compact data rows, generous decision surfaces.
- Empty states explain the next step instead of only saying "no data".

### Page anatomy

1. **Command hero**: state, next action, 1 primary CTA, 0-1 secondary CTA.
2. **Signal row**: at most three to five compact metrics.
3. **Work surface**: table, form, chat, or live log. This is the visual anchor.
4. **Assist rail**: guidance, examples, caveats, secondary metadata.
5. **Disclosure**: long docs, raw technical output, and low-frequency settings.

### Visual hierarchy rules

- Only one `work-surface` card per page whenever possible.
- Use `command-hero` for pages with a clear action, `operator-brief` for live overview, and `inventory-command` for resource pools.
- If a page contains a form and help text, the form is the left/primary surface; help text becomes a sticky right rail.
- If a page contains docs/instructions, turn long text blocks into `guide-path` steps and code blocks into copyable command cards.
- Do not put more than three equally prominent cards above the fold.

### Interaction rules

- Place filters in the header of the table they affect.
- Use row-level actions only for the row object.
- Prefer disclosure panels for implementation details.
- Any generated secret must have an explicit copy action and short-lived visibility.
- Any destructive action stays visually secondary unless it is the only task of the page.

## 8. Applied analysis of current Admin UI

### Before

- Dashboard mixed KPIs, chart, Provider status, account cards, and logs with similar visual weight.
- Accounts used five summary cards plus filters plus table, creating a stacked layout with no obvious first action.
- Empty traffic state consumed too much visual priority.
- The operator had to infer whether to investigate accounts, traffic, or logs.

### After

- Dashboard starts with an `Operator brief`: current state and recommended order.
- KPI cards became a `Signal strip`, reducing competition with the primary decision.
- Accounts starts with an `Inventory command`: health score, issue breakdown, and processing order.
- The account table is now the primary work surface, while controls support filtering and search.
- OAuth, enrollment, Kiro Gateway, remote chat, and guide pages follow the same grammar:
  one command hero, one dominant work surface, and a right-side assist rail or progressive disclosure.

## 9. Implementation map

| Pattern | CSS class | Main use |
| --- | --- | --- |
| Overview decision | `operator-brief` | Dashboard |
| Resource command | `inventory-command` | Accounts |
| Page command | `command-hero` | Groups, Keys, Settings, OAuth, Kiro, Guide |
| Main work object | `work-surface` | Tables, forms, chat, token management |
| Help / setup path | `assist-rail`, `guide-path`, `guide-step-card` | Forms, docs, onboarding |
| Compact status | `signal-strip`, `hero-metrics`, `summary-grid` | Secondary metrics only |

## 10. Page-specific acceptance criteria

- **Dashboard**: in 3 seconds the operator knows whether action is needed.
- **Accounts**: the account table is primary; issues and filters are above it, not scattered.
- **Groups / Keys / Tenants**: the primary list is the work surface; explanation is below or beside it.
- **OAuth / Enrollment**: the page shows the exact next step, not a wall of provider details.
- **Guide**: docs become a guided implementation path, with code blocks grouped under clear headings.
- **Remote Chat**: API Key, model, and message are the visible task; account binding stays out of the chat surface.
- **Kiro Gateway**: status and token table are primary; acquisition instructions are assistance, not page hero.

## 11. Acceptance checklist

## 12. V14 visual telemetry rules

This pass turns cold numbers into visual state before exposing raw tables.

Benchmarks used for the rule set:

- Cloudflare Custom Dashboards: choose chart types by question — timeseries for trend, bar for comparison, donut/percentage for ratios, and drill down from high-level anomalies to logs.
  - https://developers.cloudflare.com/analytics/custom-dashboards/
- Apple UI Design Dos and Don'ts: keep primary content readable, preserve spacing and alignment, and keep controls near the content they modify.
  - https://developer.apple.com/design/tips/
- Cloudscape Service Dashboards: dashboards should let users monitor health, investigate issues, and act quickly.
  - https://cloudscape.design/patterns/general/service-dashboard/
- Ant Design: enterprise information architecture should use data display, copywriting, layout, and dark-mode guidance consistently.
  - https://ant.design/docs/spec/introduce/

Derived implementation rules:

1. **Every numeric summary needs shape.** Use `viz-ring`, `viz-meter`, `viz-severity`, or `viz-spark-bars` before the table.
2. **Use flow cards for mental models.** Any page that represents routing, permissions, enrollment, or network policy starts with a 3-node flow.
3. **Only real data gets charts.** Decorative placeholders are allowed only for empty-state skeletons; telemetry charts must map to server values or live API responses.
4. **Normal state stays calm.** Green/blue visuals are compact; amber/red states are the only elements allowed to dominate.
5. **Tables become drill-down, not the hero.** Tables remain for precision and actions, but the first read should be visual state plus next action.

## 13. V15 calm chrome and material-linked controls

The top of each page must behave like system chrome, not like page content.

Additional button/control references:

- Apple Buttons: use a prominent style for the most likely action, but keep prominent buttons to one or two per view; too many prominent buttons increase cognitive load.
  - https://developer.apple.com/design/human-interface-guidelines/buttons
- Fluent 2 Button guidance: if there are many minor actions, use outline/subtle/transparent appearances to avoid a busy layout.
  - https://fluent2.microsoft.design/components/web/react/core/button/usage
- Atlassian Button: buttons communicate what action will happen next.
  - https://atlassian.design/components/button/
- Cloudscape Button: cloud console actions should remain predictable and componentized.
  - https://cloudscape.design/components/button/

Applied rules:

1. **Topbar is background, not content.** It uses low-opacity glass, muted text, no shadow, and compact controls.
2. **Page context strips are quiet.** `page-hero command-hero` is a context breadcrumb; metrics become small context chips, not hero cards.
3. **Primary is scoped.** Topbar primary buttons use a soft accent fill; form submission and modal confirmation can use stronger filled buttons.
4. **Controls link to the surface behind them.** Buttons use translucent material, soft borders, and hover glows derived from the page mesh/accent instead of detached saturated gradients.
5. **Motion is tactile, not decorative.** Hover changes material and border; large translate/lift effects are removed from core buttons.

## 14. V17 calm telemetry and exception-first contrast

This pass applies a stricter cloud-console rule seen across Apple, Cloudscape,
Cloudflare-style operational surfaces, and enterprise design systems: **normal
state is background context; exceptions earn contrast**.

Applied rules:

1. **Healthy is quiet.** Green is no longer a dominant decorative color for
   normal quota, health, and latency. Healthy visual fills use `--ok-quiet` /
   `--viz-ok`; warning and error keep stronger contrast.
2. **Charts are supporting evidence.** Lines, rings, meters, and mini-bars use
   thinner strokes, lower opacity fills, and calmer provider colors. A chart
   should not out-rank the current task unless it contains a warning/error.
3. **Cards are surfaces, not posters.** Default visualization cards use softer
   borders, no hover lift, and a 1px low-opacity accent edge. Only warn/error
   cards are allowed to look visually “loud”.
4. **Stable dashboard actions are secondary.** In stable state the dashboard
   primary action is rendered as a quiet material control; alert state can still
   promote a stronger action.
5. **Color is semantic, not decorative.** Provider colors and success badges are
   kept for recognition, but saturation is reduced so the main work surface
   remains dominant.

For every Admin page:

- [ ] Can a user identify the next action in 3 seconds?
- [ ] Is there only one dominant above-the-fold surface?
- [ ] Are repetitive resources in rows/tables instead of cards?
- [ ] Are advanced analytics below or collapsed?
- [ ] Is every status color paired with text?
- [ ] Do controls live next to the object they affect?
- [ ] Does an empty state explain what happens next?

## 12. Gap analysis against Cloudflare / Tencent Cloud / Alibaba Cloud / Apple

### What is still different

- **Cloudflare-style clarity**: our dashboard and account pool now expose the state and next action, but several secondary pages still carry long explanatory hero copy. The new `compact` hero class reduces this, and future pages should default to compact unless they are true operator overview pages.
- **Tencent/Alibaba-style enterprise consistency**: the Admin SSR pages now share `command-hero`, `work-surface`, `assist-rail`, `metric-card`, and `disclosure-card`, but historical CSS still contains multiple generations of component tokens. Future refactors should consolidate duplicate KPI/card primitives after functional work stabilizes.
- **Apple-style focus**: Login and Remote Chat are now closer to a single-task surface. Portal Dashboard still exposes multiple client examples; this pass makes OpenAI/Codex primary and hides the secondary examples visually to reduce first-screen density.
- **Visualization depth**: Dashboard has charts and provider bars; Accounts uses health/issue visualization; Audit still relies mostly on table/log streams. Future work should add an error trend strip and severity distribution when audit data volume grows.

### Changes landed in this pass

- Remote Chat: removed the fixed response-account configuration block from the page, leaving one chat work surface.
- Audit: made historical events the primary surface and moved live SSE to a secondary rail.
- Portal Dashboard: removed duplicate KPI card wall and replaced the empty API Key block with an inline next-step prompt.
- Groups: compressed hero density and changed system prompt from long text to configured/not-configured status.
- Keys: moved integration examples into a disclosure panel.
- Form/settings/OAuth/enrollment/portal pages: switched to compact command heroes to reduce page-top text density.
