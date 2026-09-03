# Plan — bus de conversation Claude Code ↔ Codex

## 1. Objectif et contraintes

**Contrainte ferme** : plusieurs instances de Claude Code et de Codex CLI doivent pouvoir converser entre elles, de façon visible et pilotable par un humain.

**Choix libres** : toute la stack est ouverte. Indépendant de T3 Code (mais compatible : T3 hérite de la config native des providers).

**Principe directeur** : *pas d'agent qui attend*. Chaque message adressé à un agent déclenche un tour sur sa session existante. Cela évacue le problème du réveil (MCP est pull-only, un agent idle ne reçoit rien) sans polling, sans long-poll, sans hooks obligatoires.

**Corollaire** : un agent ne peut pas obtenir de réponse *pendant* son tour. Demander = terminer son tour par un message adressé au pair avec le tag `[ask]`. La réponse du pair déclenche le tour suivant.

**Choix de transport** : pas d'IRC ni de Matrix. Un seul binaire Go = bus + driver. Ce qu'un ircd apportait (clients tout faits) n'est pas dans le périmètre v1 ; ce qu'il coûtait (512 octets/ligne, flood limits, SASL, une connexion par nick, paste pour les sorties longues) disparaît.

**Hors périmètre v1** : lead agent orchestrateur, agents interactifs (terminal/T3) dans le channel, multi-humain, clients natifs.

## 2. Architecture

```
  humain (navigateur, curl)
        │  POST /msg · GET /events (SSE)
        ▼
  ┌──────────────────────────────┐   claude -p --resume     ┌────────────┐
  │           airc               │ ───────────────────────▶ │ Claude Code│
  │  Go · SQLite · HTTP · UI     │   codex exec resume      │   Codex    │
  │  (bus + driver, un process)  │ ◀─────────────────────── └────────────┘
  └──────────────────────────────┘             ▲
        │  POST ntfy sur [ask] / [waiting]     │ curl POST /msg (say, allowed tool)
        ▼                                      │
   téléphone                                   └── depuis le tour de l'agent
```

Tourne sur la machine de dev : c'est là que vivent les CLI, les worktrees et l'auth des providers.

### 2.1 Bus
- SQLite, une table `messages {id, ts, channel, from, to, body, tags}`. `to` = nick adressé ou vide.
- `POST /msg {channel, from, to, body}` — auth par token (header). `from` humain ou nick d'agent.
- `GET /events?channel=` — SSE, replay depuis `?since=<id>`.
- `GET /` — page HTML statique : liste des messages, filtre par channel, textarea, rendu markdown côté client (diffs en `<pre>`).
- Un channel par tâche (`task-<id>`), `lobby` pour le contrôle. Topic = première ligne du channel.

### 2.2 Driver — le réveil
- Table `agents {nick, provider, channel, session_id, cwd, sandbox, tools, last_msg_id}` dans le même SQLite.
- Sur un message avec `to = nick` :
  1. construit le prompt = **prompt protocole** (§3) + messages du channel depuis `last_msg_id` (delta, pas de rejeu) + message ;
  2. exécute selon le provider :
     - Claude : `claude -p --resume <session_id> --append-system-prompt <protocole> --allowedTools <…>`, prompt sur stdin
     - Codex : `codex exec resume <thread_id> -c sandbox_mode=… -`, protocole + prompt sur stdin
  3. insère la réponse dans le bus sous le nick de l'agent, parse les tags de fin de tour.
- Une file par nick : les tours d'une même session sont sérialisés.
- Commandes humaines (messages `!…` dans n'importe quel channel) : `!spawn <nick> <provider> <channel> <cwd> [--sandbox ro|rw] [--tools …]`, `!stop <nick>`, `!kill <nick>`, `!status`, `!release [n]`, `!lock`, `!unlock`.
- Notification : `[ask]` adressé à un humain ou `[waiting for human]` → POST ntfy.

### 2.3 `say` — la parole pendant le tour
- Pas de MCP. `curl -X POST localhost:<port>/msg` dans les `tools` de l'agent, token en env.
- Sert à publier de l'avancement ou interpeller un pair mid-tour. Ne permet pas de recevoir : le pair répond dans le bus, l'agent le verra à son prochain réveil.

## 3. Conventions

| Élément | Convention |
|---|---|
| Nick | `<provider>-<rôle>[-<n>]` : `claude-impl`, `codex-review`, `claude-lead-2` |
| Channel | `task-<id>` par tâche ; `lobby` pour le contrôle |
| Adressage | champ `to` — seul déclencheur de réveil. Dérivé de la première ligne commençant par `@nick` (n'importe où dans le corps) |
| Demander | terminer le tour par un message adressé + `[ask]` ; la réponse réveille |
| Tags de fin de tour | `[done]`, `[ask]`, `[blocked]`, `[waiting for human]` |
| Longueur | libre. Markdown, blocs de code. Le résumé en tête reste court pour l'humain |
| DM | pas de DM : tout passe par le channel de la tâche |

**Prompt protocole** : injecté par le driver à chaque tour, il porte ces conventions à l'agent (adressage, tags, pairs présents dans le channel, usage de `say`). Claude : `--append-system-prompt`. Codex : préfixe du prompt (pas de flag équivalent sur `exec`).

## 4. Garde-fous (v1, non négociables)

- Réveil uniquement sur adressage explicite ; jamais sur le bruit du channel.
- Cap bot→bot : N messages consécutifs d'agents dans un channel sans message humain (défaut 4), puis `[waiting for human]` et pause. Un message humain remet le compteur à zéro. `!release <n>` (bouton + nombre dans l'UI) libère n tours au-delà du cap et rejoue le réveil mis en pause. `!lock` / `!unlock` gèle un channel.
- Budget par tâche : nombre max de tours par nick et par channel, coupure automatique.
- Max de durée de tour ; dépassement → kill du process, `[blocked]` reposté.
- Sécurité : les messages d'un autre agent sont de l'**input non fiable** pour un agent avec accès disque.
  - Codex : `approval_policy = "never"` (obligatoire en `exec`, sinon échec), `sandbox_mode = "read-only"` pour un reviewer, `"workspace-write"` pour un implémenteur, `network_access` explicite si besoin.
  - Claude : en `-p`, un tool non pré-autorisé est refusé, pas demandé. `--permission-mode auto` par défaut en `rw` (classifieur Claude Code), `default` en `ro` ; `tools` par nick, liste explicite (`Bash(git:*)`, `Bash(make:*)`, `Bash(curl:*)`, sans espaces). Jamais `bypassPermissions`.
- Le bus écoute sur `localhost` uniquement. Exposition externe = derrière l'ingress/oauth-proxy existant, jamais en direct.
- Chaque instance dans son propre worktree, sauf décision explicite au `!spawn`. Deux agents sur le même arbre : un seul en écriture.

## 5. Phases

### Phase 0 — Vérifications (½ journée)
- [ ] `claude -p --output-format json` : `session_id` renvoyé et réutilisable avec `--resume` ; ou fixer l'id à la création via `--session-id`.
- [ ] `claude -p --allowedTools` : confirmer qu'un tool hors liste est refusé silencieusement, et la syntaxe des patterns `Bash(…)`.
- [ ] `claude -p --append-system-prompt` avec `--resume` : le prompt protocole est bien pris en compte à chaque tour.
- [ ] `codex exec` : forme exacte de `resume <thread_id>`, output structuré (`--json` ?), et moyen d'injecter le prompt protocole (`AGENTS.md`, flag, config).

### Phase 1 — Bus (½ journée)
- [ ] Binaire Go, SQLite, `POST /msg`, `GET /events` SSE avec replay, token.
- [ ] Page HTML statique : liste, filtre channel, textarea, markdown.
- [ ] Test : deux onglets, un message dans l'un apparaît dans l'autre.

### Phase 2 — Driver (1–2 jours)
- [ ] Table `agents`, `!spawn` : crée la session (Claude via `--session-id`, Codex via premier `codex exec` puis capture du thread id).
- [ ] Réveil sur `to`, file par nick, insertion de la réponse, parse des tags.
- [ ] Prompt protocole + delta de contexte depuis `last_msg_id`.
- [ ] `!stop`, `!status`, `!kill`.
- [ ] `say` : token en env de l'agent, `curl` dans `tools`.

### Phase 3 — Garde-fous (½ journée)
- [ ] Cap bot→bot, budget par tâche, max durée de tour, tags `[waiting for human]`.
- [ ] Notification ntfy sur `[ask]` humain / `[waiting for human]`.
- [ ] Logs structurés (qui a réveillé qui, durée, tokens si disponibles).

### Phase 4 — Scénario de validation
Un seul scénario, de bout en bout :
1. `!spawn claude-impl claude task-1 /wt/task-1 --sandbox rw`
2. `!spawn codex-review codex task-1 /wt/task-1 --sandbox ro`
3. Humain → `claude-impl` : `implémente X, demande à codex-review de relire avant de commit`
4. Attendu : Claude travaille, publie un `say` d'avancement, termine son tour par `@codex-review diff prêt [ask]` ; le driver réveille Codex ; Codex répond `@claude-impl … [done]` ; le driver réveille Claude avec ce delta ; Claude commit, `[done]` ; ntfy reçu.
5. Intervention humaine au milieu (`codex-review: sois plus strict sur les tests`) prise en compte.

## 6. Extensions (après v1)

- **Agents interactifs** (terminal, T3) dans le channel : hook `Stop` / frontière de tour côté Claude qui lit le delta via `GET /events` ; équivalent côté Codex à vérifier (support des hooks non confirmé).
- **Lead agent** : un Claude Code qui joue le rôle du driver humain. À évaluer seulement après avoir vu le mode pair tourner.
- **Transport externe** (IRC, Matrix) : si un jour des clients natifs ou du multi-humain deviennent nécessaires, un pont bus ↔ transport. Le driver ne change pas.
- **Mémoire cross-projet** : le channel comme source d'index pour le serveur de mémoire MCP.

## 7. Risques identifiés

| Risque | Mitigation |
|---|---|
| Boucle bot→bot | Cap d'échanges + budget par tâche |
| Prompt injection inter-agents | Sandbox stricts, `tools` minimaux, pas de bypass, worktrees isolés, bus sur localhost |
| Agent qui ignore le protocole (pas de tag, pas d'adressage) | Prompt protocole à chaque tour ; sans adressage personne ne se réveille, l'humain relance |
| Session Claude/Codex perdue ou corrompue | `!spawn` idempotent ; resume échoue → nouvelle session, contexte = topic + log du channel |
| Tour trop long | Max durée, kill, `[blocked]` |
| Double quota consommé | Visible dans les logs, budget par tâche |
| Évolution des CLI (`-p`, `exec`, flags) | Isoler l'invocation par provider derrière une interface dans le driver |

## 8. Non vérifié à ce stade

- Syntaxe exacte `codex exec resume`, output JSON, injection d'instructions par tour.
- Comportement `--allowedTools` et `--append-system-prompt` en `claude -p --resume`.
- Support des hooks côté Codex (source secondaire uniquement).
