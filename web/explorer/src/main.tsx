import { StrictMode } from "react";
import { createRoot } from "react-dom/client";

import { AppShell } from "./AppShell";
import "./styles.css";

const rootElement = document.getElementById("root");

if (!rootElement) {
  throw new Error("Explorer root element is missing.");
}

createRoot(rootElement).render(
  <StrictMode>
    <AppShell />
  </StrictMode>,
);
