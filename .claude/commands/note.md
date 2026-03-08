# /note

Inscrit immédiatement une information dans le vault via les outils MCP.

## Usage

/note <texte libre>

Exemples :
  /note AFP meeting repoussé au 15
  /note no CGO — modernc uniquement pour tout SQLite
  /note Scan() ne doit jamais être appelée depuis un goroutine
  /note todo : écrire les tests de SessionStart, bloqué par CI [high]

## Comportement

Claude traduit le texte en appel `vault_upsert_entity` structuré :
- choisit le type le plus proche de ce qui existe déjà
- extrait label, blob_plus, blob_minus si applicable
- type=todo si la note est une tâche
- confirme ce qui a été inscrit en une ligne

## Instructions

Tu es invoqué via la commande /note. L'utilisateur te fournit du texte libre
après la commande. Tu dois :

1. Lire le texte fourni : $ARGUMENTS
2. Vérifier les entités existantes : `vault_search_entities({"namespace": "<projet>", "limit": 10})`
3. Traduire le texte en `vault_upsert_entity` avec le type le plus approprié
4. Confirmer en une ligne ce qui a été inscrit
