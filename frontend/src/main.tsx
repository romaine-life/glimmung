import React from "react";
import ReactDOM from "react-dom/client";
import { BrowserRouter } from "react-router-dom";
import { App } from "./App";
import { installMockFetch } from "./mockApi";
import "./index.css";
import "./mature.css";

installMockFetch();

// Mature theme defaults. Flip to "rounded" / "comfortable" to compare, or wire
// these to a user setting. mature.css responds to both attributes live.
document.documentElement.dataset.geometry = "chamfer";
document.documentElement.dataset.density = "compact";

ReactDOM.createRoot(document.getElementById("root")!).render(
  <React.StrictMode>
    <BrowserRouter>
      <App />
    </BrowserRouter>
  </React.StrictMode>
);
