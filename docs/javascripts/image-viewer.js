// Click a screenshot to see it at full size. Each image is wrapped in a link to
// its file, which makes it keyboard reachable and leaves ctrl-click and
// open-in-new-tab working; a plain click opens the modal viewer instead.
(function () {
  let viewer;

  function createViewer() {
    const dialog = document.createElement("dialog");
    dialog.className = "image-viewer";
    dialog.setAttribute("aria-label", "Full size image");

    // The scroller takes the initial focus so the arrow keys pan a large image.
    const scroller = document.createElement("div");
    scroller.className = "image-viewer__scroller";
    scroller.tabIndex = 0;
    scroller.autofocus = true;
    const image = document.createElement("img");
    scroller.append(image);

    const close = document.createElement("button");
    close.type = "button";
    close.className = "image-viewer__close";
    close.setAttribute("aria-label", "Close");
    close.textContent = "×";

    dialog.append(scroller, close);
    dialog.addEventListener("click", () => dialog.close());
    dialog.addEventListener("close", () => image.removeAttribute("src"));
    document.body.append(dialog);
    return { dialog, image };
  }

  function open(source) {
    viewer ??= createViewer();
    viewer.image.src = source.src;
    viewer.image.alt = source.alt;
    viewer.dialog.showModal();
  }

  function enhance(image) {
    const link = document.createElement("a");
    link.className = "image-viewer-link";
    link.href = image.src;
    link.title = "View full size";
    link.addEventListener("click", (event) => {
      if (event.ctrlKey || event.metaKey || event.shiftKey || event.altKey) return;
      event.preventDefault();
      open(image);
    });
    image.replaceWith(link);
    link.append(image);
  }

  // document$ fires on the first load and after every instant navigation, whose
  // new page content needs wrapping and which can arrive with the viewer open
  // through the browser's back button.
  document$.subscribe(() => {
    viewer?.dialog.close();
    document.querySelectorAll(".md-content img").forEach((image) => {
      if (!image.closest("a")) enhance(image);
    });
  });
})();
