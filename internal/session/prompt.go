package session

import "ttc/internal/prompts"

const systemTemplate = prompts.System

// childSystemTemplate keeps child behavior stable without embedding an actor ID.
const childSystemTemplate = systemTemplate + prompts.Child
