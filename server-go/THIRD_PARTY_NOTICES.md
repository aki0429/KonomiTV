# Third-party notices

## Chrome for Testing

The optional `browser-assets` command downloads Chrome for Testing 157.0.8103.78 directly
from Google's official distribution. The browser is not embedded in this repository.
The command verifies fixed SHA-256 digests before extraction and preserves the archive
contents, including Google's `ABOUT` copyright notice. Chrome's bundled notices and
terms are available at `chrome://credits` and `chrome://terms`.

- Distribution: https://googlechromelabs.github.io/chrome-for-testing/
- License information: https://www.google.com/chrome/terms/
- Linux amd64 SHA-256: `9223832977a13db3646201b1341c1f9cf08afa3ceae92540116d7b0c062364fb`
- Windows amd64 SHA-256: `5f6ec40206407112702e163bf694d07f9fdc8ccbd346c70ce61fa62f716493f9`
- Linux arm64 SHA-256: `f354d666822a11d8fe99e923c44be77dd226fab2750c5b5947e6318818bf10f1`

The optional Ubuntu dependency step uses the host's configured APT repositories to
download `libasound2t64`, `libgbm1`, and `fonts-noto-cjk`. Packages are extracted into
the application's browser directory. Their copyright and license documents are kept
under `libraries/usr/share/doc/`; no OS packages or security policies are modified.

## OpenCV thumbnail image processing and face detection

The Go thumbnail cascade evaluator, rectangle grouping, exact linear and area resize and
Canny edge detector are adapted from OpenCV / OpenCV Contrib:

- https://github.com/opencv/opencv_contrib/tree/5.x/modules/xobjdetect/src
- https://github.com/opencv/opencv/tree/5.x/modules/imgproc/src

Copyright (C) 2000, Intel Corporation, all rights reserved.
Copyright (C) 2008-2013, Itseez Inc., all rights reserved.
Copyright (C) 2014, Itseez Inc., all rights reserved.
Third party copyrights are property of their respective owners.

Redistribution and use in source and binary forms, with or without modification,
are permitted provided that the following conditions are met:

- Redistributions of source code must retain the above copyright notice,
  this list of conditions and the following disclaimer.
- Redistributions in binary form must reproduce the above copyright notice,
  this list of conditions and the following disclaimer in the documentation
  and/or other materials provided with the distribution.
- The names of Intel Corporation and Itseez Inc. may not be used to endorse or
  promote products derived from this software without specific prior written
  permission.

THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS "AS IS" AND
ANY EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT LIMITED TO, THE IMPLIED
WARRANTIES OF MERCHANTABILITY AND FITNESS FOR A PARTICULAR PURPOSE ARE DISCLAIMED.
IN NO EVENT SHALL THE COPYRIGHT HOLDERS OR CONTRIBUTORS BE LIABLE FOR ANY DIRECT,
INDIRECT, INCIDENTAL, SPECIAL, EXEMPLARY, OR CONSEQUENTIAL DAMAGES (INCLUDING,
BUT NOT LIMITED TO, PROCUREMENT OF SUBSTITUTE GOODS OR SERVICES; LOSS OF USE,
DATA, OR PROFITS; OR BUSINESS INTERRUPTION) HOWEVER CAUSED AND ON ANY THEORY OF
LIABILITY, WHETHER IN CONTRACT, STRICT LIABILITY, OR TORT (INCLUDING NEGLIGENCE
OR OTHERWISE) ARISING IN ANY WAY OUT OF THE USE OF THIS SOFTWARE, EVEN IF ADVISED
OF THE POSSIBILITY OF SUCH DAMAGE.

## Bundled face detection models

`internal/metadata/data/haarcascade_frontalface_default.xml` is the same model as
the pinned upstream KonomiTV source. Created by Rainer Lienhart.
Copyright (C) 2000, Intel Corporation, all rights reserved. It is distributed
under the Intel Open Source Computer Vision Library license printed above;
the complete original notice is also retained in the XML.

`internal/metadata/data/lbpcascade_animeface.xml` is the same model as the pinned
upstream KonomiTV source. Copyright (C) 2011 by nagadomi@nurs.or.jp.

The MIT License (MIT)

Permission is hereby granted, free of charge, to any person obtaining a copy of
this software and associated documentation files (the "Software"), to deal in
the Software without restriction, including without limitation the rights to
use, copy, modify, merge, publish, distribute, sublicense, and/or sell copies of
the Software, and to permit persons to whom the Software is furnished to do so,
subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.

## Test photographs

`internal/metadata/testdata/thumbnail_scoring/astronaut.png` and `face-*.png` /
`posterized-face-*.png` are resized/cropped derivatives of the public-domain
NASA photograph of Eileen Collins, distributed by scikit-image v0.25.2 as
`skimage/data/astronaut.png`. No known copyright restrictions.

- https://github.com/scikit-image/scikit-image/blob/v0.25.2/skimage/data/_fetchers.py
- https://flic.kr/p/r9qvLn

## purego

The thumbnail worker uses github.com/ebitengine/purego v0.10.2 to call the public
FFmpeg API without a C compiler. Copyright the Ebitengine authors.
The Apache License 2.0 is reproduced in licenses/purego-Apache-2.0.txt.
Source: https://github.com/ebitengine/purego/tree/v0.10.2

## Thumbnail FFmpeg libraries

The optional thumbnail-assets command downloads the pinned PyAV 19.0.2 wheel
listed in cmd/thumbnail-assets/libraries.json and verifies its SHA-256. It extracts
only av.libs and the original distribution metadata and notices. No Python module
is executed or installed. The source bundle is downloaded directly from PyPI;
shared libraries are not checked into this repository. Keep the extracted notices
and assets-manifest.json with a redistributed installation.

PyAV source and wheel build configuration: https://github.com/PyAV-Org/PyAV/tree/v19.0.2
FFmpeg source matching these libraries: https://github.com/FFmpeg/FFmpeg/tree/n9.0.2
FFmpeg licensing and component information: https://ffmpeg.org/legal.html
