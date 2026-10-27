// Fixture: a component the library must never contain. It moves on the wall
// clock and through the CSS engine, so two renders of frame N would differ.
Components["wobble"] = {
  mount: function (el) { var n = document.createElement("div"); n.style.transition = "opacity 300ms ease-in-out"; return { root: n }; },
  update: function (state) { state.root.style.left = (Date.now() % 100) + "px"; state.root.style.width = "540px"; }
};
