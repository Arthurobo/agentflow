import "@testing-library/jest-dom/vitest";
import { afterEach } from "vitest";
import { cleanup } from "@testing-library/react";

// Vitest runs without globals, so React Testing Library's auto-cleanup is not
// registered; without this each render accumulates into the same jsdom document
// and role queries start matching stale elements across tests.
afterEach(() => cleanup());