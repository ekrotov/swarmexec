/* Copy buttons for command blocks: an element with data-copy="…" gets a
   button that puts that text on the clipboard. The button is created here,
   so without JavaScript — or without the asynchronous Clipboard API (an
   insecure origin, an old browser) — there is none, and the command is still
   there to select. The stylesheet reserves the button's room on [data-copy]
   in any case, so nothing moves when it appears. */
(function () {
  "use strict";
  if (!navigator.clipboard || !navigator.clipboard.writeText || !window.isSecureContext) return;
  document.querySelectorAll("[data-copy]").forEach(function (block) {
    var text = block.getAttribute("data-copy");
    var btn = document.createElement("button");
    btn.type = "button";
    btn.className = "copybtn";
    btn.setAttribute("aria-label", block.getAttribute("data-copy-label") || "Copy the command");
    btn.textContent = "Copy";
    var status = document.createElement("span");
    status.className = "copystatus";
    status.setAttribute("role", "status");
    var timer = 0;
    btn.addEventListener("click", function () {
      navigator.clipboard.writeText(text).then(function () {
        btn.textContent = "Copied";
        btn.classList.add("done");
        status.textContent = "Copied to the clipboard.";
      }, function () {
        btn.textContent = "Failed";
        status.textContent = "Could not copy; select the command instead.";
      }).then(function () {
        clearTimeout(timer);
        timer = setTimeout(function () {
          btn.textContent = "Copy";
          btn.classList.remove("done");
          status.textContent = "";
        }, 2000);
      });
    });
    block.classList.add("has-copy");
    block.appendChild(btn);
    block.appendChild(status);
  });
})();
