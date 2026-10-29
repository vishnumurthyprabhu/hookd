// Copy buttons: a button with data-copy="ID" copies the text of the element
// with that ID. The buttons are hidden until this script shows them, so the
// pages work without JavaScript.
document.addEventListener("DOMContentLoaded", function () {
  document.querySelectorAll("button[data-copy]").forEach(function (button) {
    var target = document.getElementById(button.dataset.copy);
    if (!target) {
      return;
    }
    button.hidden = false;
    button.addEventListener("click", function () {
      var text = target.textContent;
      var done = function () {
        button.textContent = "Copied";
        setTimeout(function () { button.textContent = "Copy"; }, 2000);
      };
      if (navigator.clipboard && window.isSecureContext) {
        navigator.clipboard.writeText(text).then(done, function () { select(target); });
      } else {
        select(target);
        try {
          if (document.execCommand("copy")) {
            done();
          }
        } catch (e) {
          // The text stays selected for the user to copy.
        }
      }
    });
  });

  function select(el) {
    var range = document.createRange();
    range.selectNodeContents(el);
    var sel = window.getSelection();
    sel.removeAllRanges();
    sel.addRange(range);
  }
});
