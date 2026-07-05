// Browser bootstrap for mounting the React console application.

import React from "react";
import { createRoot } from "react-dom/client";
import { App } from "./app";
import "./styles.css";

// Vite serves index.html with a required #root element; fail fast if that contract changes.
createRoot(document.getElementById("root")!).render(
  <React.StrictMode>
    <App />
  </React.StrictMode>
);
