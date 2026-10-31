# Library panel

The library is the panel on the right of the [Groups](dashboard-groups.md) tab.
It holds the reusable pieces you start agents from: profiles, roles, saved
prompts and templates. **Hide ›** or the chevron on its edge folds it away, and
the dashboard remembers that.

## What's in it

From top to bottom:

| Section | What an item is |
|---|---|
| 👤 **Agent profiles** | A saved spawn setup: harness, model, sandbox and so on. See [Spawning and lifecycle](spawning-and-lifecycle.md). |
| 🎭 **Roles** | Guidance and permissions for a job, such as `dev` or `reviewer` |
| 📝 **Saved prompts** | Reusable task text; see [below](#saved-prompts) |
| 🧩 **Group templates** | A blueprint for a team. See [Teams at scale](teams-at-scale.md). |
| ◈ **Processes** | Saved process templates, with `features.processes` on. See [Processes](processes.md). |
| 🛡 **Sandbox profiles** | What agents may reach. See [Sandboxing](sandboxing.md). |
| 💰 **Usage profiles** | Budgets you can apply to agents and groups |
| 🚦 **Usage limits & alerts** | Spending caps and quota guards. See [Usage and costs](usage-and-costs.md). |

## Using it

- **Start an agent.** Drag a profile, role or template onto a group to open the
  spawn dialog with it filled in. Dragging a process onto a group pins it there.
- **Find something.** The filter box searches every section. Each word must match
  an item's name or one of its values, such as a model, a path or a line of a
  prompt. A word naming a section, such as `roles`, keeps that whole section.
- **Section buttons.** **＋** creates an item, **⧉** opens the section's manager,
  and **⋯** has **Import…** and **Export all**. Usage sections have no **⋯**. You
  can also drop a file on a section to import it.
- **Card ⚙ menu.** **Edit**, **Clone**, **Rename…**, **Pin to top**, **Copy name**,
  **Export** and **Delete**. Templates add **Deploy…**. Agent, sandbox and usage
  profiles add **Set as default**. Processes offer **Copy id** and **Export .pwf**.
  Usage profiles and limits can't be renamed or exported.

Renaming changes only the name: whatever uses the item follows it. A process
rename is saved as its next version. A pinned item leads its section.

Exports are config bundles, the same format as `tclaude config export`. Paths are
written relative to home, and suspected credentials stop the export until you
confirm. Importing opens a review of every item (new, replaces existing or
unchanged), where you can leave items out, allow replacing, and say where the
bundle's paths are on this machine. A section's older export file opens that
section's own importer. Processes also share as `.pwf`; see
[Portable Workflow Format](pwf.md).

## Agent profiles

A profile saves a spawn setup so you don't fill in the form each time. Set a
default with **Set as default** on a card, or set one per group with 🧠 on the
group. See
[how the defaults combine](spawning-and-lifecycle.md).

## Saved prompts

A saved prompt is reusable task text, such as a cold-review brief. A role, in
contrast, gives an agent guidance and permissions. Prompts are shared across all
groups on this daemon.

Pick one or more as chips under **Saved prompts** in the spawn dialog. Their text
opens the new agent's task, in order, ahead of what you type in **Initial msg**.
The total can't exceed 16,384 bytes.

### Where prompts come from

The first level that names any prompts wins; nothing merges:

1. The spawn itself: the chips, or `tclaude agent spawn --prompt <name>`.
2. The spawn profile: **Saved prompts** in the profile editor. A named profile
   beats the group's default profile.
3. The group: **Saved prompts** in its `⚙ settings…`. The global default profile
   is the last fallback.

The dialog fills in its chips in this order, so what it shows is what the agent
gets. Removing every chip spawns with none. Non-interactive spawns only use
prompts they name, and interactive shell spawns take none.

Template deploys, cron jobs, triggers and process workers follow the same idea:
their profile's prompts, else the group's. They go ahead of the agent's
instruction.

### Managing prompts

**⧉** on the section opens the manager, which creates prompts and exports or
imports them as JSON. An import shows which names are new and skips existing ones
unless you choose to overwrite those that differ.

In a terminal, **✉ Message** has an **Insert prompt…** picker, which puts a
prompt's text at your cursor for editing and doesn't send anything.

Names are short handles, such as `cold-review`. Renaming updates every profile
and group using the prompt. Deleting is refused while any still use it.

### Prompts in bundles

Spawn profile and template exports include the prompts their profiles name, and
group exports include the group's default prompts. Importing creates missing
prompts and never overwrites a local one with the same name: it keeps yours and
says so. Agent `.paf` bundles leave saved prompts out.

### For agents

Agents can read prompts through `GET /v1/saved-prompts`. Changing them needs
`prompts.manage`, which is not granted by default. Setting a group's default
prompts needs the group startup-context permission.
