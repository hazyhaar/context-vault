# /note

Inscrit immédiatement une information en DB via prends-note.

## Usage

/note <texte libre>

Exemples :
  /note AFP meeting repoussé au 15
  /note no CGO — modernc uniquement pour tout SQLite
  /note Scan() ne doit jamais être appelée depuis un goroutine
  /note todo : écrire les tests de SessionStart, bloqué par CI [high]

## Comportement

Claude traduit le texte en INSERT structuré :
- choisit le type le plus proche de ce qui existe déjà
- extrait label, blob_plus, blob_minus si applicable
- type=todo si la note est une tâche
- confirme ce qui a été inscrit en une ligne

## Instructions

Tu es invoqué via la commande /note. L'utilisateur te fournit du texte libre
après la commande. Tu dois :

1. Lire le texte fourni : $ARGUMENTS
2. Invoquer le skill prends-note pour persister l'information
3. Avant d'inscrire, vérifier les types et clés existants dans la DB
4. Traduire le texte en INSERT structuré avec le type le plus approprié
5. Confirmer en une ligne ce qui a été inscrit
