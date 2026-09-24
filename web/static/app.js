// The site works without JavaScript. This file only makes two things nicer:
//
// 1. Photos are shrunk in the browser to at most 2048px before upload, so a
//    12 MB phone photo costs a few hundred KB of cellular data instead. The
//    server re-processes every photo anyway (orientation, metadata, size), so
//    this is purely about upload size.
// 2. Delete and Remove buttons ask "are you sure?".
(function () {
  "use strict";
  var MAX = 2048;

  function shrink(file) {
    if (!file.type || file.type.indexOf("image/") !== 0 || !window.createImageBitmap) {
      return Promise.resolve(file);
    }
    // imageOrientation: "from-image" applies the EXIF rotation while drawing.
    return createImageBitmap(file, { imageOrientation: "from-image" }).then(function (bmp) {
      var scale = Math.min(1, MAX / Math.max(bmp.width, bmp.height));
      if (scale === 1 && file.size < 2 * 1024 * 1024) {
        return file; // already small enough
      }
      var c = document.createElement("canvas");
      c.width = Math.round(bmp.width * scale);
      c.height = Math.round(bmp.height * scale);
      c.getContext("2d").drawImage(bmp, 0, 0, c.width, c.height);
      return new Promise(function (resolve) {
        c.toBlob(function (b) { resolve(b ? new File([b], "photo.jpg", { type: "image/jpeg" }) : file); }, "image/jpeg", 0.9);
      });
    }).catch(function () { return file; });
  }

  document.addEventListener("submit", function (e) {
    var form = e.target;
    if (form.classList.contains("confirm") && !window.confirm(form.getAttribute("data-confirm") || "Are you sure?")) {
      e.preventDefault();
      return;
    }
    if (!form.classList.contains("resize") || !window.fetch || !window.FormData) {
      return;
    }
    var inputs = form.querySelectorAll('input[type="file"]');
    var any = false;
    inputs.forEach(function (i) { if (i.files && i.files.length) { any = true; } });
    if (!any) {
      return;
    }
    e.preventDefault();
    var button = form.querySelector("button");
    if (button) { button.disabled = true; button.textContent = "Uploading…"; }
    var data = new FormData(form);
    var jobs = [];
    inputs.forEach(function (input) {
      data.delete(input.name);
      Array.prototype.forEach.call(input.files, function (f) {
        jobs.push(shrink(f).then(function (s) { return [input.name, s]; }));
      });
    });
    Promise.all(jobs).then(function (files) {
      files.forEach(function (nf) { data.append(nf[0], nf[1], nf[1].name || "photo.jpg"); });
      return fetch(form.action, { method: "POST", body: data, credentials: "same-origin" });
    }).then(function (resp) {
      if (resp.redirected) {
        window.location = resp.url;
        return;
      }
      return resp.text().then(function (html) {
        document.open(); document.write(html); document.close();
      });
    }).catch(function () {
      form.submit(); // fall back to a plain upload
    });
  });
})();
