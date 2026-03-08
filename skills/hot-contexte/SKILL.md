---
name: hot-contexte
description: >-
  Refresh de contexte léger depuis vault.db. Raisonne sur l'état réel,
  complète la DB si nécessaire, retourne un état ciblé (~300 tokens).
  Trois cas : avant /compact manuel, quand Claude dérive, changement de sujet.
---

# Skill : hot-contexte

## Quand invoquer

1. **Avant `/compact` manuel** — prépare la DB pour que `buildStartContext` restitue fidèlement après compactage
2. **Quand Claude dérive ou hallucine** — refresh léger sans relire le transcript, recadrage en ~300 tokens
3. **Changement de sujet en session longue** — recentre sur ce qui est en DB sans polluer le contexte

L'utilisateur ou Claude peut invoquer cette skill à tout moment.
Elle n'est **jamais** déclenchée automatiquement par un hook.

## Étape 1 — Raisonnement explicite (ne pas sauter)

Réponds à ces questions avant d'interroger le vault :

1. **Objectif en cours** — Où en est-on exactement ?
2. **Bloquants** — Qu'est-ce qui est bloquant ou fragile ?
3. **Décisions récentes** — Quelles décisions seraient perdues sans la DB ?
4. **Fichiers chauds** — Quels fichiers sont modifiés ou sur le point de l'être ?
5. **Contraintes implicites** — Y a-t-il des contraintes apprises en session et pas encore inscrites ?

## Étape 2 — Compléter si nécessaire

Si le raisonnement révèle des trous → invoquer `prends-note` pour inscrire avant de continuer.

```
vault_upsert_entity({
  "namespace": "<namespace>",
  "type": "decision",
  "label": "<décision manquante>",
  "meta": {"blob_plus": "...", "blob_minus": "..."}
})
```

## Étape 3 — Lecture ciblée

Vue globale des todos, décisions et contraintes actifs :

```
vault_get_context({"namespace": "<namespace>"})
```

Si besoin de plus de détails — recherche ciblée :

```
# Tous les todos avec blockers
vault_list_todos({"namespace": "<namespace>"})

# Recherche par label
vault_search_entities({"query": "<zone chaude>", "namespace": "<namespace>"})

# Recherche par type
vault_search_entities({"type": "decision", "namespace": "<namespace>"})
```

## Invariants

- Commence **toujours** par le raisonnement, **jamais** par une requête
- `sensitivity=2` est automatiquement exclu des résultats MCP
- Après complétion (étape 2), toujours relire (étape 3) pour confirmer

## Relation avec buildStartContext

`buildStartContext` (dans le binaire) est le **lecteur** post-compactage automatique.
`hot-contexte` est le **penseur** — il raisonne, complète, puis lit.
Les deux coexistent, rôles distincts.
